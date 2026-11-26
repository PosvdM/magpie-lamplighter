package providers

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// Credential is the credential of one account, read from Magpie by the
// host. Fetch and Profile read the fields their provider needs.
type Credential = host.Credential

func scalarString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	}
	return ""
}
