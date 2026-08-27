package project_test

import (
	"testing"

	"github.com/b42labs/tally/internal/core/project"
)

func TestIsVirtualPlatform(t *testing.T) {
	tests := []struct {
		name     string
		platform string
		want     bool
	}{
		{name: "meta", platform: "meta", want: true},
		{name: "partner", platform: "partner", want: true},
		{name: "empty", platform: ""},
		{name: "a real platform", platform: "openstack"},
		{name: "wrong case", platform: "Meta"},
		{name: "trailing space", platform: "partner "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := project.IsVirtualPlatform(tc.platform); got != tc.want {
				t.Errorf("IsVirtualPlatform(%q) = %t, want %t", tc.platform, got, tc.want)
			}
		})
	}
}

func TestIsVirtualRelationType(t *testing.T) {
	tests := []struct {
		name         string
		relationType string
		want         bool
	}{
		{name: "member_of", relationType: "member_of", want: true},
		{name: "managed_by", relationType: "managed_by", want: true},
		{name: "empty", relationType: ""},
		{name: "an attributing relation", relationType: "infrastructure_tenant"},
		{name: "wrong case", relationType: "Member_of"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := project.IsVirtualRelationType(tc.relationType); got != tc.want {
				t.Errorf("IsVirtualRelationType(%q) = %t, want %t", tc.relationType, got, tc.want)
			}
		})
	}
}
