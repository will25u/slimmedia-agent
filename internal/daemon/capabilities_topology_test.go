package daemon

import (
	"encoding/json"
	"testing"

	"github.com/will25u/slimmedia-agent/internal/ffmpeg"
	"github.com/will25u/slimmedia-agent/internal/hardware"
	"github.com/will25u/slimmedia-agent/internal/safety"
)

// TestBuildCapabilitiesReportIncludesTopology (issue #201): the startup
// hardware audit must report the detected CPU topology — hybrid core
// classes, multi-CCD layout, or definitively symmetric.
func TestBuildCapabilitiesReportIncludesTopology(t *testing.T) {
	caps := hardware.Probe()
	report := BuildCapabilitiesReport(caps, ffmpeg.EncoderCaps{}, safety.MediaServerPresence{})

	if report.Hardware.CPU.Topology == nil {
		t.Fatal("audit report missing cpu topology")
	}
	topo := report.Hardware.CPU.Topology
	if topo.Symmetric == (topo.Hybrid || topo.CCDs > 1) {
		t.Fatalf("symmetric verdict disagrees with steering fields: %+v", topo)
	}

	b, err := json.Marshal(report.Hardware.CPU)
	if err != nil {
		t.Fatalf("marshal cpu section: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal cpu section: %v", err)
	}
	if _, ok := decoded["topology"]; !ok {
		t.Fatalf("cpu section JSON missing topology key: %s", b)
	}
}
