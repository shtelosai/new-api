package service

import (
	"bufio"
	"encoding/base64"
	"errors"
	_ "golang.org/x/image/webp"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/types"
	"github.com/tidwall/gjson"
)

// 图片渠道收到明确的服务端故障后冷却；参数、额度、权限和内容拒绝不惩罚渠道。
func IsTworkImageChannelFailure(err *types.NewAPIError) bool {
	if err == nil || types.IsSkipRetryError(err) || (err.GetErrorCode() == "image_result_invalid" || err.GetErrorCode() == "image_size_mismatch") {
		return false
	}
	oai := err.ToOpenAIError()
	if isHardModelHealthError(normalizeModelHealthText(oai.Type),
		normalizeModelHealthText(openAIErrorCodeToString(oai.Code)), normalizeModelHealthText(oai.Message)) {
		return false
	}
	return err.StatusCode == 429 || err.StatusCode >= 500 && err.StatusCode <= 599
}

// ValidateTworkImageResult 在专用图片链路输出和结算前排除空图、文字和损坏响应。
func ValidateTworkImageResult(body []byte, gemini bool) error {
	validBase64 := func(value string) bool {
		reader := bufio.NewReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(value)))
		config, _, err := image.DecodeConfig(reader)
		if err != nil || config.Width <= 0 || config.Height <= 0 {
			return false
		}
		_, err = io.Copy(io.Discard, reader)
		return err == nil
	}
	if gemini {
		for _, candidate := range gjson.GetBytes(body, "candidates").Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				image := part.Get("inlineData")
				if !image.Exists() {
					image = part.Get("inline_data")
				}
				mime := image.Get("mimeType").String()
				if mime == "" {
					mime = image.Get("mime_type").String()
				}
				if strings.HasPrefix(mime, "image/") && validBase64(image.Get("data").String()) {
					return nil
				}
			}
		}
	} else {
		for _, image := range gjson.GetBytes(body, "data").Array() {
			if validBase64(image.Get("b64_json").String()) {
				return nil
			}
			parsed, err := url.Parse(image.Get("url").String())
			if err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" {
				return nil
			}
		}
	}
	return errors.New("图片上游没有返回有效图片")
}
