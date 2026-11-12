package workflow

import (
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/config"
)

func TestDeprecatedAnalyzerTemperatureStillLoads(t *testing.T) {
	store := config.NewStore(nil)
	store.SetTemporary(config.KeyWorkflowAnalyzerTemp, "0.2")
	got := config.NewBuilder(store, nil).GetWorkflowConfig().AnalyzerTemperature
	if got != 0.2 {
		t.Fatalf("stored analyzer temperature = %v", got)
	}
	var desc string
	for _, spec := range config.WorkflowMetaSpecs() {
		if spec.Key == config.KeyWorkflowAnalyzerTemp {
			desc = spec.Description
		}
	}
	if !strings.Contains(strings.ToLower(desc), "deprecated") {
		t.Fatalf("description %q does not mark the key deprecated", desc)
	}
}
