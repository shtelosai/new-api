package service

import (
	"bytes"
	"encoding/base64"
	"github.com/stretchr/testify/assert"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestValidateTworkImageResult(t *testing.T) {
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(buffer.Bytes())
	for _, tc := range []struct {
		body          string
		gemini, valid bool
	}{
		{`{"data":[]}`, false, false},
		{`{"data":[{"b64_json":"aW1hZ2U="}]}`, false, false},
		{`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}}]}`, true, false},
		{`{"data":[{"b64_json":"!!!"}]}`, false, false},
		{`{"data":[{"url":"https://cdn.example/image.png"}]}`, false, true},
		{`{"data":[{"url":"file:///etc/passwd"}]}`, false, false},
		{`{"data":[{"b64_json":"$IMAGE"}]}`, false, true},
		{`{"candidates":[{"content":{"parts":[{"text":"done"}]}}]}`, true, false},
		{`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"$IMAGE"}}]}}]}`, true, true},
		{`{"candidates":[{"content":{"parts":[{"inline_data":{"mime_type":"image/jpeg","data":"$IMAGE"}}]}}]}`, true, true},
	} {
		tc.body = strings.ReplaceAll(tc.body, "$IMAGE", encoded)
		t.Run(tc.body, func(t *testing.T) {
			assert.Equal(t, tc.valid, ValidateTworkImageResult([]byte(tc.body), tc.gemini) == nil)
		})
	}
}
