package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// 企业模型授权由 tbackend 实时校验；签名仅委托本次企业 token 的精确请求，不写编译模型白名单。
func resolveEmployeeChatRoute(c *gin.Context, billingModel string) (*model.Channel, bool, error) {
	const header = "X-Twork-Employee-Authorization"
	values, present := c.Request.Header[http.CanonicalHeaderKey(header)]
	if !present {
		return nil, false, nil
	}
	// 服务间凭证不得传递到模型提供方。
	defer c.Request.Header.Del(header)
	denied := model.ErrTworkRouteDenied
	key := os.Getenv("TWORK_EMPLOYEE_CHAT_BRIDGE_KEY")
	channelID := c.GetString(string(constant.ContextKeyTokenSpecificChannelId))
	tokenID := c.GetInt(string(constant.ContextKeyTokenId))
	path := c.Request.URL.Path
	if len(values) != 1 || len(key) < 32 || tokenID <= 0 || !tworkChannelIDPattern.MatchString(channelID) || c.Request.Method != http.MethodPost || (path != "/v1/responses" && path != "/v1/chat/completions") {
		return nil, true, denied
	}
	parts := strings.Split(values[0], ".")
	if len(parts) != 2 {
		return nil, true, denied
	}
	stamp, err := strconv.ParseInt(parts[0], 10, 64)
	now := time.Now().Unix()
	if err != nil || strconv.FormatInt(stamp, 10) != parts[0] || stamp < now-60 || stamp > now+5 {
		return nil, true, denied
	}
	proof, err := hex.DecodeString(parts[1])
	if err != nil || len(proof) != sha256.Size {
		return nil, true, denied
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, true, denied
	}
	body, err := storage.Bytes()
	if err != nil {
		return nil, true, denied
	}
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "twork-employee-chat-v1\n%s\nPOST\n%s\n%d\n%s\n%x", parts[0], path, tokenID, channelID, sha256.Sum256(body))
	if !hmac.Equal(proof, mac.Sum(nil)) {
		return nil, true, denied
	}
	requestedModel, exists, err := common.CanonicalJSONStringField(body, "model")
	if err != nil || !exists || requestedModel == "" || model.DB == nil {
		return nil, true, denied
	}
	id, err := strconv.Atoi(channelID)
	if err != nil {
		return nil, true, denied
	}
	db := model.DB.WithContext(c.Request.Context())
	var channel model.Channel
	if err := db.First(&channel, id).Error; err != nil || channel.Status != common.ChannelStatusEnabled {
		return nil, true, denied
	}
	wire, err := channel.TworkWireAPI()
	if err != nil || (wire == "responses" && path != "/v1/responses") || (wire != "responses" && path != "/v1/chat/completions") || !channelSupportsRequestPath(&channel, path, requestedModel) {
		return nil, true, denied
	}
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	groups := strings.Split(channel.Group, ",")
	for i := range groups {
		groups[i] = strings.TrimSpace(groups[i])
	}
	if !slices.Contains(strings.Split(channel.Models, ","), requestedModel) || !slices.Contains(groups, group) {
		return nil, true, denied
	}
	var disabled int64
	if err := db.Model(&model.ChannelModelDisabled{}).Where("channel_id = ? AND model IN ?", id, []string{requestedModel, billingModel}).Count(&disabled).Error; err != nil || disabled > 0 {
		return nil, true, denied
	}
	if _, err := storage.Seek(0, io.SeekStart); err != nil {
		return nil, true, denied
	}
	c.Request.Body = io.NopCloser(storage)
	return &channel, true, nil
}
