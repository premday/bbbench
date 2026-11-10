package rungroup

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLabelJSONTags(t *testing.T) {
	b, _ := json.Marshal(Label{Setting: "s", Value: "v", Valid: true})
	for _, k := range []string{`"setting"`, `"value"`, `"before"`, `"after"`, `"valid"`, `"note"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("missing %s in %s", k, b)
		}
	}
}
