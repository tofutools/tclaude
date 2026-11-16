package processcmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// --print writes a run's template as YAML that --file reads back unchanged.
func TestAmendPrintRoundTripsTheBuiltinTemplates(t *testing.T) {
	paths, err := filepath.Glob("../process/builtin/templates/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no builtin templates: %v", err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := model.ParseAuthoring(data)
		if err != nil || len(parsed.Diagnostics.Errors()) > 0 {
			t.Fatalf("%s: %v %v", path, err, parsed.Diagnostics.Errors())
		}
		out, err := yaml.Marshal(parsed.Template)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		again, err := model.ParseAuthoring(out)
		if err != nil || len(again.Diagnostics.Errors()) > 0 {
			t.Fatalf("%s: printed template does not parse: %v %v\n%s", path, err, again.Diagnostics.Errors(), out)
		}
		if !reflect.DeepEqual(parsed.Template, again.Template) {
			t.Fatalf("%s: printed template differs after parsing it back", path)
		}
	}
}
