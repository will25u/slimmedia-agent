package hardware

import "testing"

func TestCountRelationship(t *testing.T) {
	tests := []struct {
		name         string
		records      []logicalProcessorInfo
		relationship uint32
		want         int
	}{
		{"empty buffer", nil, relationProcessorCore, 0},
		{
			"counts only core records",
			[]logicalProcessorInfo{
				{relationship: relationProcessorCore},
				{relationship: 2}, // RelationCache
				{relationship: relationProcessorCore},
				{relationship: 3}, // RelationProcessorPackage
				{relationship: relationProcessorCore},
			},
			relationProcessorCore,
			3,
		},
		{
			"other relationship",
			[]logicalProcessorInfo{
				{relationship: relationProcessorCore},
				{relationship: 2},
				{relationship: 2},
			},
			2,
			2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countRelationship(tt.records, tt.relationship); got != tt.want {
				t.Errorf("countRelationship(%v, %d) = %d, want %d", tt.records, tt.relationship, got, tt.want)
			}
		})
	}
}
