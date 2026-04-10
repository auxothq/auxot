package builtins

import (
	"strings"
	"testing"
)

func TestRegistryInit(t *testing.T) {
	content, ok := LookupSkill("auxot:openui")
	if !ok {
		t.Fatal("expected auxot:openui to be registered")
	}
	if !strings.Contains(content, "OpenUI Lang") {
		t.Error("skill content missing expected text")
	}
}

func TestLookupSkillFromArgs(t *testing.T) {
	_, ok := LookupSkillFromArgs(`{"skill_id": "auxot:openui"}`)
	if !ok {
		t.Fatal("expected auxot:openui to be found via args lookup")
	}
	_, ok2 := LookupSkillFromArgs(`{"skill_id": "other:something"}`)
	if ok2 {
		t.Fatal("non-auxot skill should not be found")
	}
}

func TestAppendAuxotOpenUIChatFooterIfNeeded(t *testing.T) {
	base := "skill body"
	out := AppendAuxotOpenUIChatFooterIfNeeded("auxot:openui", base)
	if !strings.Contains(out, "statically") || !strings.HasPrefix(out, base) {
		t.Fatalf("expected footer appended: %q", out)
	}
	if AppendAuxotOpenUIChatFooterIfNeeded("auxot:other", base) != base {
		t.Fatal("other auxot skills should not get openui footer")
	}
}
