package operation_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetSoftFailureCooldownSeconds(t *testing.T) {
	original := channelHealthSetting
	t.Cleanup(func() { channelHealthSetting = original })

	tests := []struct {
		name    string
		seconds int
		want    int
	}{
		{name: "zero falls back to default", seconds: 0, want: 30},
		{name: "negative falls back to default", seconds: -1, want: 30},
		{name: "positive value is preserved", seconds: 7, want: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channelHealthSetting.SoftFailureCooldownSeconds = tt.seconds
			assert.Equal(t, tt.want, GetSoftFailureCooldownSeconds())
		})
	}
}

func TestGetSoftFailureMaxAttempts(t *testing.T) {
	original := channelHealthSetting
	t.Cleanup(func() { channelHealthSetting = original })

	tests := []struct {
		name     string
		attempts int
		want     int
	}{
		{name: "zero falls back to default", attempts: 0, want: 5},
		{name: "negative falls back to default", attempts: -1, want: 5},
		{name: "above maximum falls back to default", attempts: 11, want: 5},
		{name: "far above maximum falls back to default", attempts: 100, want: 5},
		{name: "valid value is preserved", attempts: 3, want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channelHealthSetting.SoftFailureMaxAttempts = tt.attempts
			assert.Equal(t, tt.want, GetSoftFailureMaxAttempts())
		})
	}
}

func TestIsSoftFailureCooldownEnabled(t *testing.T) {
	original := channelHealthSetting
	t.Cleanup(func() { channelHealthSetting = original })

	channelHealthSetting.SoftFailureCooldownEnabled = true
	assert.True(t, IsSoftFailureCooldownEnabled())

	channelHealthSetting.SoftFailureCooldownEnabled = false
	assert.False(t, IsSoftFailureCooldownEnabled())
}
