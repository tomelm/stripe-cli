package observe

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsLiveModeAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  string
		live bool
	}{
		{key: "sk_live_abc123", live: true},
		{key: "rk_live_abc123", live: true},
		{key: "pk_live_abc123", live: true},
		{key: "sk_test_abc123", live: false},
		{key: "rk_test_abc123", live: false},
		{key: "pk_test_abc123", live: false},
		{key: "", live: false},
		{key: "garbage", live: false},
	}
	for _, test := range tests {
		assert.Equal(t, test.live, IsLiveModeAPIKey(test.key), "key %q", test.key)
	}
}
