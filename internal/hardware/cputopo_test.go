package hardware

import (
	"reflect"
	"testing"
)

func TestClassifyIntelModels(t *testing.T) {
	tests := []struct {
		name       string
		vendor     string
		models     []int
		wantHybrid bool
		wantClass  []coreClass
	}{
		{
			name:       "alder lake hybrid",
			vendor:     "GenuineIntel",
			models:     []int{154, 154, 151, 151},
			wantHybrid: true,
			wantClass:  []coreClass{classEfficiency, classEfficiency, classPerformance, classPerformance},
		},
		{
			name:   "symmetric single model",
			vendor: "GenuineIntel",
			models: []int{151, 151, 151, 151},
		},
		{
			name:   "amd ignored",
			vendor: "AuthenticAMD",
			models: []int{97, 97, 154, 151},
		},
		{
			name:   "unknown vendor",
			vendor: "",
			models: []int{154, 151},
		},
		{
			name:   "unknown model in hybrid set degrades to symmetric",
			vendor: "GenuineIntel",
			models: []int{151, 254},
		},
		{
			name:   "empty",
			vendor: "GenuineIntel",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hybrid, classes := classifyIntelModels(tt.vendor, tt.models)
			if hybrid != tt.wantHybrid {
				t.Fatalf("hybrid = %v, want %v", hybrid, tt.wantHybrid)
			}
			if !reflect.DeepEqual(classes, tt.wantClass) {
				t.Fatalf("classes = %v, want %v", classes, tt.wantClass)
			}
		})
	}
}

func TestTopologyFromClasses(t *testing.T) {
	// 4 P-cores with HT (cpus 0-3 and 8-11 = two threads each) + 4 E-cores.
	classes := []coreClass{
		classPerformance, classPerformance, classPerformance, classPerformance,
		classEfficiency, classEfficiency, classEfficiency, classEfficiency,
		classPerformance, classPerformance, classPerformance, classPerformance,
	}
	groups := []string{"0,8", "1,9", "2,10", "3,11", "4", "5", "6", "7", "0,8", "1,9", "2,10", "3,11"}

	topo := topologyFromClasses(classes, groups)
	if !topo.Hybrid {
		t.Fatal("hybrid = false")
	}
	if topo.PerformanceCores != 4 || topo.EfficiencyCores != 4 {
		t.Fatalf("cores = %dP/%dE, want 4P/4E", topo.PerformanceCores, topo.EfficiencyCores)
	}
	wantCPUs := []int{0, 1, 2, 3, 8, 9, 10, 11}
	if !reflect.DeepEqual(topo.PerformanceCPUs, wantCPUs) {
		t.Fatalf("performance CPUs = %v, want %v", topo.PerformanceCPUs, wantCPUs)
	}
}

func TestFinalizeTopologySymmetricVerdict(t *testing.T) {
	if got := finalizeTopology(CPUTopology{}); !got.Symmetric {
		t.Error("zero topology should be symmetric")
	}
	if got := finalizeTopology(CPUTopology{CCDs: 2}); got.Symmetric {
		t.Error("multi-CCD topology must not be symmetric")
	}
	if got := finalizeTopology(CPUTopology{Hybrid: true}); got.Symmetric {
		t.Error("hybrid topology must not be symmetric")
	}
}

func TestParseCPUList(t *testing.T) {
	tests := []struct {
		in   string
		want []int
	}{
		{"0-3", []int{0, 1, 2, 3}},
		{"0-3,8,12-14", []int{0, 1, 2, 3, 8, 12, 13, 14}},
		{"5", []int{5}},
		{"", nil},
		{"junk,1-2", []int{1, 2}},
		{"7-3", nil},
	}
	for _, tt := range tests {
		if got := parseCPUList(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseCPUList(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestFormatCPUList(t *testing.T) {
	tests := []struct {
		in   []int
		want string
	}{
		{[]int{0, 1, 2, 3}, "0-3"},
		{[]int{0, 1, 2, 3, 8, 12, 13, 14}, "0-3,8,12-14"},
		{[]int{5}, "5"},
		{nil, ""},
		{[]int{3, 1, 2, 0}, "0-3"},
	}
	for _, tt := range tests {
		if got := formatCPUList(tt.in); got != tt.want {
			t.Errorf("formatCPUList(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCPUListRoundTrip(t *testing.T) {
	for _, in := range []string{"0-7,16-23", "0-3,8,12-15", "0-63", "4"} {
		if got := formatCPUList(parseCPUList(in)); got != in {
			t.Errorf("round trip of %q = %q", in, got)
		}
	}
}
