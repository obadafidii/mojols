package main

import (
	"reflect"
	"testing"
)

func TestBackendArgs(t *testing.T) {
	got := backendArgs(includeDirs([]string{"/a"}, []string{"/b", "/a", "/c"}), []string{"--log=verbose"})
	want := []string{"-I", "/a", "-I", "/b", "-I", "/c", "--log=verbose"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
