package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
)

type imageAdapterStatus struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	ErrorCode      string `json:"error_code"`
	Retryable      bool   `json:"retryable"`
	ActualSize     string `json:"actual_size"`
	OutputFormat   string `json:"output_format"`
	ImageCount     int    `json:"image_count"`
	ProviderTaskID string `json:"provider_task_id"`
	ResultExpired  bool   `json:"result_expired"`
}

func (s *ImageJobService) RunOnce(ctx context.Context) error {
	if !model.ImageJobAccountingSupported() {
		return errors.New("异步图片金额同步暂不可用")
	}
	// 即使暂无图片任务，仍恢复暂态失败的额度移交；单个主体失败不能饿死其他用户。
	reconcileErr := model.ReconcileImageJobAccounting(ctx)
	job, err := model.ClaimImageJob(ctx, uuid.NewString(), time.Now().Unix(), s.LeaseSeconds)
	if err != nil {
		return err
	}
	if job != nil {
		workCtx, cancel := context.WithTimeout(ctx, time.Duration(s.LeaseSeconds-10)*time.Second)
		err = s.advance(workCtx, job)
		cancel()
		_ = model.UpdateImageJobLease(ctx, job, map[string]any{"lease_owner": "", "lease_until": 0, "next_poll_at": time.Now().Add(s.PollInterval).Unix()})
		if err != nil {
			return err
		}
	}
	if err := model.ProjectImageJobLogs(ctx); err != nil {
		return err
	}
	if err := s.cleanup(ctx); err != nil {
		return err
	}
	return reconcileErr
}

func (s *ImageJobService) advance(ctx context.Context, job *model.ImageJob) error {
	if job.Status == "settling" {
		picture, err := os.ReadFile(filepath.Join(s.DataDir, "results", job.ID))
		if err != nil || len(picture) > 64<<20 || int64(len(picture)) != job.Bytes || fmt.Sprintf("%x", sha256.Sum256(picture)) != job.ResultHash {
			return errors.New("图片结果尚未恢复，暂不结算")
		}
		return model.SettleImageJob(ctx, job.ID, true, "", job.LeaseOwner)
	}
	payload, err := os.ReadFile(filepath.Join(s.DataDir, "requests", job.ID))
	if err != nil {
		return err
	}
	var request model.ImageJobRequest
	if len(payload) > 64<<20 || common.Unmarshal(payload, &request) != nil {
		return errors.New("图片持久请求损坏")
	}
	digest, err := request.Hash()
	if err != nil || digest != job.RequestHash {
		return errors.New("图片持久请求校验失败")
	}
	var channel *model.Channel
	if job.Status == "queued" {
		channels, err := model.ImageJobChannels(ctx, job.TokenID, request)
		if err != nil {
			if errors.Is(err, model.ErrImageJobAuth) || errors.Is(err, model.ErrTworkRouteDenied) {
				return model.SettleImageJob(ctx, job.ID, false, "image_auth_revoked", job.LeaseOwner)
			}
			return err
		}
		var attempted []int
		if common.UnmarshalJsonStr(job.AttemptedChannels, &attempted) != nil {
			return errors.New("图片渠道尝试记录损坏")
		}
		for _, candidate := range channels {
			tried := false
			for _, id := range attempted {
				if id == candidate.Id {
					tried = true
					break
				}
			}
			if !tried {
				channel = candidate
				break
			}
		}
		if channel == nil {
			return model.SettleImageJob(ctx, job.ID, false, "image_no_channel", job.LeaseOwner)
		}
		provider, family, err := channel.ImageJobProvider()
		if err != nil {
			return err
		}
		attempted = append(attempted, channel.Id)
		encoded, _ := common.Marshal(attempted)
		if err = model.UpdateImageJobLease(ctx, job, map[string]any{"status": "submitting", "channel_id": channel.Id, "adapter_base_url": *channel.BaseURL, "image_provider": provider, "model_family": family, "attempted_channels": string(encoded)}); err != nil {
			return err
		}
		job.Status = "submitting"
		job.ChannelID = channel.Id
		job.AdapterBaseURL = *channel.BaseURL
		job.ImageProvider = provider
		job.ModelFamily = family
		job.AttemptedChannels = string(encoded)
		return s.submit(ctx, job, request, channel)
	}
	channel, err = model.GetChannelById(job.ChannelID, true)
	if err != nil {
		return err
	}
	// 已受理任务始终查原 adapter + job_id；渠道撤权只阻止下一次提交，不把已受理任务转投备用。
	response, status, err := s.adapterRequest(ctx, channel, imageJobEndpoint(job, channel)+"/"+job.ID, "GET", nil, 64<<10)
	if err != nil {
		return err
	}
	if status == 404 && (job.Status == "submitting" || job.Status == "unknown") && job.ProviderTaskID == "" {
		// adapter 对 route+job_id 持久幂等；网络中断只重放同一接收标识，禁止新 ID/换渠道。
		authorized, err := model.ImageJobChannels(ctx, job.TokenID, request)
		if err != nil {
			return err
		}
		for _, candidate := range authorized {
			if candidate.Id == channel.Id && candidate.BaseURL != nil && *candidate.BaseURL == job.AdapterBaseURL {
				return s.submit(ctx, job, request, channel)
			}
		}
		return model.UpdateImageJobLease(ctx, job, map[string]any{"status": "unknown", "error_code": "image_auth_revoked"})
	}
	if status != 200 {
		return errors.New("图片任务查询暂不可用")
	}
	var result imageAdapterStatus
	if common.Unmarshal(response, &result) != nil || result.ID != job.ID {
		return errors.New("图片任务响应身份不一致")
	}
	return s.applyStatus(ctx, job, request, channel, result)
}

func (s *ImageJobService) submit(ctx context.Context, job *model.ImageJob, r model.ImageJobRequest, ch *model.Channel) error {
	body := map[string]any{"job_id": job.ID, "model": "gpt-image-2.5-" + job.ModelFamily, "prompt": r.Prompt, "size": r.Size, "resolution": r.Resolution, "background": r.Background}
	if r.OutputFormat != "" {
		body["output_format"] = r.OutputFormat
	}
	if r.OutputCompression != nil {
		body["output_compression"] = *r.OutputCompression
	}
	if r.Quality != "" {
		body["quality"] = r.Quality
	}
	if len(r.ImageURLs) > 0 {
		body["image_urls"] = r.ImageURLs
	}
	if r.MaskURL != "" {
		body["mask_url"] = r.MaskURL
	}
	encoded, err := common.Marshal(body)
	if err != nil {
		return err
	}
	response, status, err := s.adapterRequest(ctx, ch, imageJobEndpoint(job, ch), "POST", encoded, 64<<10)
	if err != nil {
		return model.UpdateImageJobLease(ctx, job, map[string]any{"status": "unknown", "error_code": "image_submission_unknown"})
	}
	if status == 202 || status == 200 {
		var result imageAdapterStatus
		if common.Unmarshal(response, &result) != nil || result.ID != job.ID {
			return model.UpdateImageJobLease(ctx, job, map[string]any{"status": "unknown", "error_code": "image_submission_unknown"})
		}
		return s.applyStatus(ctx, job, r, ch, result)
	}
	var problem struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if common.Unmarshal(response, &problem) == nil && problem.Error.Retryable && imageJobDefinitelyNotAccepted(problem.Error.Code) {
		return model.UpdateImageJobLease(ctx, job, map[string]any{"status": "queued", "error_code": "", "provider_task_id": ""})
	}
	// 非 2xx 不等价于未受理。只将明确的校验/认证拒绝结为失败，模糊响应保持 unknown。
	if status == 400 || status == 401 || status == 403 || status == 422 {
		return model.SettleImageJob(ctx, job.ID, false, "image_request_invalid", job.LeaseOwner)
	}
	return model.UpdateImageJobLease(ctx, job, map[string]any{"status": "unknown", "error_code": "image_submission_unknown"})
}

func imageJobDefinitelyNotAccepted(code string) bool {
	switch code {
	case "apimart_submit_rejected", "kie_submit_rejected", "image_input_unavailable", "image_upload_failed", "unsupported_image_options":
		return true
	}
	return false
}

func (s *ImageJobService) applyStatus(ctx context.Context, job *model.ImageJob, r model.ImageJobRequest, ch *model.Channel, result imageAdapterStatus) error {
	if result.ResultExpired {
		// 过期不是上游失败证明；旧适配器也可能对受理未知任务返回过期标记。
		if result.Status == "unknown" {
			return model.UpdateImageJobLease(ctx, job, map[string]any{"status": "unknown", "error_code": "image_submission_unknown"})
		}
		return model.SettleImageJob(ctx, job.ID, false, "image_result_expired", job.LeaseOwner)
	}
	switch result.Status {
	case "failed", "canceled":
		if result.Retryable && result.ProviderTaskID == "" && job.ProviderTaskID == "" && imageJobDefinitelyNotAccepted(result.ErrorCode) {
			return model.UpdateImageJobLease(ctx, job, map[string]any{"status": "queued", "error_code": "", "provider_task_id": ""})
		}
		return model.SettleImageJob(ctx, job.ID, false, imageJobSafeError(result.ErrorCode), job.LeaseOwner)
	case "queued", "submitting", "running", "unknown", "downloading":
		status := "running"
		if result.Status == "unknown" {
			status = "unknown"
		}
		return model.UpdateImageJobLease(ctx, job, map[string]any{"status": status, "provider_task_id": result.ProviderTaskID, "error_code": ""})
	case "succeeded":
		if result.ImageCount != 1 {
			return model.SettleImageJob(ctx, job.ID, false, "image_result_invalid", job.LeaseOwner)
		}
		if err := model.UpdateImageJobLease(ctx, job, map[string]any{"status": "downloading", "provider_task_id": result.ProviderTaskID}); err != nil {
			return err
		}
		picture, status, err := s.adapterRequest(ctx, ch, imageJobEndpoint(job, ch)+"/"+job.ID+"/result", "GET", nil, 64<<20)
		if err != nil || status != 200 {
			return errors.New("图片结果暂不可下载")
		}
		provider := job.ImageProvider
		if provider != "kie" && provider != "apimart" {
			return errors.New("图片原始提供方记录无效")
		}
		actual, format, err := ValidateImageJobResult(picture, r, provider)
		if err != nil {
			return model.SettleImageJob(ctx, job.ID, false, imageJobSafeError(err.Error()), job.LeaseOwner)
		}
		if (result.ActualSize != "" && result.ActualSize != actual) || (result.OutputFormat != "" && result.OutputFormat != format) {
			return model.SettleImageJob(ctx, job.ID, false, "image_result_invalid", job.LeaseOwner)
		}
		if err = s.writeImmutable("results", job.ID, picture); err != nil {
			return err
		}
		fields := map[string]any{"status": "settling", "actual_size": actual, "output_format": format, "bytes": len(picture), "result_hash": fmt.Sprintf("%x", sha256.Sum256(picture)), "error_code": ""}
		if err = model.UpdateImageJobLease(ctx, job, fields); err != nil {
			return err
		}
		return model.SettleImageJob(ctx, job.ID, true, "", job.LeaseOwner)
	default:
		return errors.New("图片任务状态无效")
	}
}
