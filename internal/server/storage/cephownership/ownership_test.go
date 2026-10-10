package cephownership

import (
	"strings"
	"testing"
)

func TestBindingValidation(t *testing.T) {
	base := Binding{Cluster: "ceph", User: "nova", FSID: "12345678-1234-1234-1234-123456789abc", PoolID: 1, ImageID: "a123b"}
	marker := "sha256:" + strings.Repeat("a", 64)
	err := base.validate(marker, marker)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		change func(*Binding)
	}{
		{"missing cluster", func(b *Binding) { b.Cluster = "" }},
		{"missing user", func(b *Binding) { b.User = "" }},
		{"negative pool", func(b *Binding) { b.PoolID = -1 }},
		{"missing FSID", func(b *Binding) { b.FSID = "" }},
		{"malformed FSID", func(b *Binding) { b.FSID = strings.Repeat("a", 36) }},
		{"missing image", func(b *Binding) { b.ImageID = "" }},
		{"image name", func(b *Binding) { b.ImageID = "container_test" }},
		{"NUL config", func(b *Binding) { b.ConfigFile = "config\x00other" }},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			binding := base
			item.change(&binding)
			if binding.validate(marker, marker) == nil {
				t.Fatal("Accepted incomplete or ambiguous binding")
			}
		})
	}

	for _, invalid := range []string{"", "owner", "sha256:" + strings.Repeat("G", 64), marker + "a"} {
		if base.validate(invalid, marker) == nil || base.validate(marker, invalid) == nil {
			t.Fatal("Accepted invalid ownership marker")
		}
	}
}
