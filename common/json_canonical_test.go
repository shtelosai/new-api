package common

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestCanonicalJSONStringFieldRejectsAmbiguousAuthorizationKeys(t *testing.T) {
	for _, tc := range []struct {
		body, key, want string
		present, valid  bool
	}{
		{`{"model":"allowed","input":{"Model":"nested"}}`, "model", "allowed", true, true},
		{`{"other":"value"}`, "model", "", false, true},
		{`{"model":"allowed","model":"forbidden"}`, "model", "", false, false},
		{`{"model":"allowed","Model":"forbidden"}`, "model", "", false, false},
		{`{"m\u006fdel":"allowed"}`, "model", "", false, false},
		{`{"model":null}`, "model", "", false, false},
		{`{"model":true}`, "model", "", false, false},
		{`{"model":[]}`, "model", "", false, false},
		{`{"twork_runtime":"codex","TWORK_RUNTIME":"legacy"}`, "twork_runtime", "", false, false},
		{`{"twork_runtime":"legacy","twork_runtime":"codex"}`, "twork_runtime", "", false, false},
		{`null`, "model", "", false, false},
		{`[]`, "model", "", false, false},
		{`{"model":"allowed"} {"model":"forbidden"}`, "model", "", false, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			value, present, err := CanonicalJSONStringField([]byte(tc.body), tc.key)
			if tc.valid {
				assert.NoError(t, err)
				assert.Equal(t, tc.want, value)
				assert.Equal(t, tc.present, present)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
