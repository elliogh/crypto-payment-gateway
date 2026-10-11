package partner

import (
	"strings"
	"testing"
)

func TestAPIKeyTransportSafety(t *testing.T) {
	for _, key := range []string{"local-alpha-key", strings.Repeat("x", 12), strings.Repeat("x", 256), "safe!#%&+/:=?@"} {
		if !ValidAPIKey(key) {
			t.Errorf("rejected transport-safe key of length %d", len(key))
		}
	}
	for _, key := range []string{"", strings.Repeat("x", 11), strings.Repeat("x", 257), " local-alpha-key", "local-alpha-key ", "local alpha key", "local-alpha-key\r\n", "local-alpha-key\t", "local-alpha-key\x00", "local-alpha-key\x7f", "локальный-ключ"} {
		if ValidAPIKey(key) {
			t.Errorf("accepted unsafe key of length %d", len(key))
		}
	}
}
