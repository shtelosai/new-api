package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// 图片固定路由与聊天路由隔离；由后端跨模型重试，网关每次只执行一个渠道。
func resolveTworkImageRoute(c *gin.Context, modelName string) (*model.Channel, bool, error) {
	values, present := c.Request.Header[http.CanonicalHeaderKey("X-Twork-Image-Channel-Id")]
	if !present {
		return nil, false, nil
	}
	invalid := errors.New("图片固定渠道请求无效")
	if len(values) != 1 || c.Request.Method != http.MethodPost {
		return nil, true, invalid
	}
	id, err := strconv.Atoi(values[0])
	if err != nil || id <= 0 || strconv.Itoa(id) != values[0] {
		return nil, true, invalid
	}
	for _, key := range []string{"X-Twork-Channel-Id", "X-Twork-Route-Mode", "X-Twork-Employee-Authorization"} {
		if _, ok := c.Request.Header[http.CanonicalHeaderKey(key)]; ok {
			return nil, true, invalid
		}
	}
	if _, ok := common.GetContextKey(c, constant.ContextKeyTokenSpecificChannelId); ok {
		return nil, true, invalid
	}
	path := c.Request.URL.Path
	openai := path == "/v1/images/generations" || path == "/v1/images/edits"
	gemini := path == "/v1beta/models/"+modelName+":generateContent" && strings.Contains(modelName, "image")
	if !openai && !gemini {
		return nil, true, invalid
	}
	// JSON 和 multipart 都拒绝重复 model，避免鉴权模型与上游实际模型不一致。
	if openai {
		if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
			if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
				return nil, true, invalid
			}
			values := c.Request.MultipartForm.Value["model"]
			if len(values) != 1 || values[0] != modelName {
				return nil, true, invalid
			}
		} else {
			storage, err := common.GetBodyStorage(c)
			if err != nil {
				return nil, true, invalid
			}
			raw, err := storage.Bytes()
			if err != nil {
				return nil, true, invalid
			}
			name, found, err := common.CanonicalJSONStringField(raw, "model")
			if err != nil || !found || name != modelName {
				return nil, true, invalid
			}
			if _, err = storage.Seek(0, io.SeekStart); err != nil {
				return nil, true, invalid
			}
			c.Request.Body = io.NopCloser(storage)
		}
	}
	channel, err := model.GetAuthorizedTworkImageChannel(c.Request.Context(), c.GetInt(string(constant.ContextKeyTokenId)), modelName, id, common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
	if err != nil {
		return nil, true, err
	}
	if openai && channel.Type != constant.ChannelTypeOpenAI || gemini && channel.Type != constant.ChannelTypeGemini {
		return nil, true, invalid
	}
	if _, cooling := service.GetChannelSoftCooldown(c, id, modelName); cooling {
		return nil, true, errors.New("图片渠道冷却中，请切换其他渠道")
	}
	common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, strconv.Itoa(id))
	common.SetContextKey(c, constant.ContextKeyTworkExplicitChannelRoute, true)
	common.SetContextKey(c, constant.ContextKeyTworkImageRoute, true)
	c.Header("X-Twork-Image-Channel-Id", strconv.Itoa(id))
	c.Request.Header.Del("X-Twork-Image-Channel-Id")
	return channel, true, nil
}

// 把后端剩余预算传到上游，不能仅依赖下游断开连接（代理可能继续持有连接）。
func tworkImageDeadline(c *gin.Context) (context.CancelFunc, error) {
	remaining := int64(280000)
	if values, ok := c.Request.Header[http.CanonicalHeaderKey("X-Twork-Image-Timeout-Ms")]; ok {
		if len(values) != 1 {
			return nil, errors.New("图片剩余时间无效")
		}
		parsed, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || parsed <= 0 || parsed > remaining {
			return nil, errors.New("图片剩余时间无效")
		}
		remaining = parsed
	}
	c.Request.Header.Del("X-Twork-Image-Timeout-Ms")
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(remaining)*time.Millisecond)
	c.Request = c.Request.WithContext(ctx)
	return cancel, nil
}
