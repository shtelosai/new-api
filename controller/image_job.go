package controller

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var imageJobPublicID = regexp.MustCompile(`^ij_[a-f0-9]{40}$`)
var imageJobClientID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func ImageJobCapabilities(c *gin.Context) {
	c.JSON(http.StatusOK, service.ImageJobCapabilities(c.Request.Context(), c.GetInt(middleware.ImageJobTokenKey)))
}
func imageJobAvailable(c *gin.Context) *service.ImageJobService {
	s := service.GetImageJobService()
	if s == nil || !model.ImageJobAccountingSupported() {
		imageJobError(c, http.StatusServiceUnavailable, "image_async_unavailable", "异步图片服务尚未启用")
		return nil
	}
	return s
}
func CreateImageJob(c *gin.Context) {
	s := imageJobAvailable(c)
	if s == nil {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<20))
	if err != nil {
		imageJobError(c, 400, "image_invalid_request", "图片请求超过大小限制")
		return
	}
	allowed := map[string]bool{"client_request_id": true, "client_session_id": true, "prompt": true, "size": true, "resolution": true, "output_format": true, "output_compression": true, "background": true, "quality": true, "image_urls": true, "mask_url": true}
	root := gjson.ParseBytes(raw)
	valid := gjson.ValidBytes(raw) && root.IsObject()
	seen := map[string]bool{}
	root.ForEach(func(key, value gjson.Result) bool {
		if !allowed[key.String()] || seen[key.String()] || key.Raw != `"`+key.String()+`"` || value.Type == gjson.Null {
			valid = false
			return false
		}
		seen[key.String()] = true
		return true
	})
	if !valid {
		imageJobError(c, 400, "image_invalid_request", "图片请求存在不支持、重复或无效参数")
		return
	}
	var request model.ImageJobRequest
	if common.Unmarshal(raw, &request) != nil {
		imageJobError(c, 400, "image_invalid_request", "图片参数类型无效")
		return
	}
	if err = request.Normalize(); err != nil {
		imageJobError(c, 400, "image_invalid_request", err.Error())
		return
	}
	job, _, err := s.Create(c.Request.Context(), c.GetInt(middleware.ImageJobTokenKey), request, c.GetHeader("X-Twork-Image-Replay-Only") == "true")
	if err != nil {
		imageJobModelError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusAccepted, job.View())
}
func GetImageJob(c *gin.Context) {
	if imageJobAvailable(c) == nil {
		return
	}
	if !imageJobPublicID.MatchString(c.Param("id")) {
		imageJobError(c, 404, "image_task_not_found", "图片任务不存在")
		return
	}
	job, err := model.GetImageJob(c.Request.Context(), c.GetInt(middleware.ImageJobTokenKey), c.Param("id"))
	if err != nil {
		imageJobModelError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, job.View())
}
func ListImageJobs(c *gin.Context) {
	if imageJobAvailable(c) == nil {
		return
	}
	session, cursor := c.Query("client_session_id"), c.Query("cursor")
	limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if limitErr != nil || limit < 1 || limit > 100 {
		imageJobError(c, 400, "image_invalid_request", "分页数量须为 1 至 100")
		return
	}
	if (session != "" && !imageJobClientID.MatchString(session)) || (cursor != "" && !imageJobPublicID.MatchString(cursor)) {
		imageJobError(c, 400, "image_invalid_request", "会话或分页标识无效")
		return
	}
	jobs, err := model.ListImageJobs(c.Request.Context(), c.GetInt(middleware.ImageJobTokenKey), session, cursor)
	if err != nil {
		imageJobModelError(c, err)
		return
	}
	next := ""
	if len(jobs) > limit {
		jobs = jobs[:limit]
		next = jobs[limit-1].ID
	}
	items := make([]model.ImageJobView, 0, len(jobs))
	for _, job := range jobs {
		items = append(items, job.View())
	}
	result := gin.H{"items": items}
	if next != "" {
		result["next_cursor"] = next
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, result)
}
func ImageJobResult(c *gin.Context) {
	s := imageJobAvailable(c)
	if s == nil {
		return
	}
	if !imageJobPublicID.MatchString(c.Param("id")) {
		imageJobError(c, 404, "image_task_not_found", "图片任务不存在")
		return
	}
	data, mime, err := s.Result(c.Request.Context(), c.GetInt(middleware.ImageJobTokenKey), c.Param("id"))
	if err != nil {
		imageJobModelError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(200, mime, data)
}
func imageJobError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
func imageJobModelError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrImageJobCreationDisabled):
		imageJobError(c, 503, "image_async_unavailable", "异步图片暂不接受新任务，已有任务仍可查询")
	case errors.Is(err, model.ErrImageJobConflict):
		imageJobError(c, 409, "image_request_conflict", "同一请求标识不能用于不同图片需求")
	case errors.Is(err, model.ErrImageJobQuota):
		imageJobError(c, 402, "image_quota_insufficient", "可用额度不足，未提交生成")
	case errors.Is(err, model.ErrTworkRouteDenied):
		imageJobError(c, 403, "image_access_denied", "没有符合本次图片要求的可用渠道")
	case errors.Is(err, model.ErrImageJobAuth):
		imageJobError(c, 401, "image_auth_invalid", "图片任务凭据已失效")
	case errors.Is(err, model.ErrImageJobNotFound):
		imageJobError(c, 404, "image_task_not_found", "图片任务不存在")
	case err.Error() == "image_result_not_ready":
		imageJobError(c, 409, "image_result_not_ready", "图片尚未生成完成")
	case err.Error() == "image_result_expired":
		imageJobError(c, 410, "image_result_expired", "图片已过期，不会重新生成")
	case err.Error() == "image_result_invalid":
		imageJobError(c, 503, "image_result_invalid", "图片校验未通过，不会重新生成")
	default:
		imageJobError(c, 503, "image_result_unavailable", "图片任务暂不可用，请稍后查询同一任务")
	}
}

// PrepareImageJobAccounting 在 tbackend 重算额度前移交旧批量增量，暂停新建时仍可调用。
func PrepareImageJobAccounting(c *gin.Context) {
	if imageJobAvailable(c) == nil {
		return
	}
	if err := model.PrepareImageJobAccounting(c.Request.Context(), c.GetInt(middleware.ImageJobTokenKey)); err != nil {
		imageJobError(c, 503, "image_async_unavailable", "图片额度准备尚未完成，请稍后重试")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"prepared": true})
}
