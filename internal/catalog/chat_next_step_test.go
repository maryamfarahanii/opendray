package catalog

import (
	"reflect"
	"testing"
)

func TestMergeAppendSystemPrompts(t *testing.T) {
	got := mergeAppendSystemPrompts([]string{"--resume", "x", "--append-system-prompt", "a", "--model", "m", "--append-system-prompt", "b"})
	want := []string{"--resume", "x", "--model", "m", "--append-system-prompt", "a\n\nb"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	none := []string{"--resume", "x"}
	if got := mergeAppendSystemPrompts(none); !reflect.DeepEqual(got, none) {
		t.Fatalf("got %q", got)
	}
}
