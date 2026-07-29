package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestRuleProfileCRUDPreservesOrderAndBuiltinMetadata(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.SaveRuleProfile(RuleProfile{
		Key: "default", Name: "常用规则", Builtin: true, FallbackAction: "proxy",
		Rules:       []RuleSelection{{Key: "geosite/apple-cn", Action: "direct"}, {Key: "geosite/apple", Action: "proxy"}},
		CustomRules: []CustomRule{{Type: "domain-suffix", Value: "example.com", Action: "direct"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := st.GetRuleProfileByKey("default")
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != id || !profile.Builtin || profile.Rules[0].Key != "geosite/apple-cn" || profile.Rules[1].Key != "geosite/apple" || profile.CustomRules[0].Type != "DOMAIN-SUFFIX" {
		t.Fatalf("rule profile was not normalized without losing order: %+v", profile)
	}
	profile.Name = "修改后"
	profile.Rules = append(profile.Rules, RuleSelection{Key: "geosite/apple", Action: "direct"})
	if _, err := st.SaveRuleProfile(profile); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetRuleProfile(id)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "修改后" || len(updated.Rules) != 2 || updated.CreatedAt != profile.CreatedAt {
		t.Fatalf("rule profile update is wrong: %+v", updated)
	}
	profiles, err := st.ListRuleProfiles()
	if err != nil || len(profiles) != 1 {
		t.Fatalf("list rule profiles: %v %+v", err, profiles)
	}
	var conflict *ResourceDeletionConflict
	if err := st.DeleteRuleProfile(id); !errors.As(err, &conflict) || conflict.Reason != resourceDeletionProtected {
		t.Fatalf("delete built-in profile conflict = %#v, err = %v", conflict, err)
	}
	customID, err := st.SaveRuleProfile(RuleProfile{Name: "自定义", FallbackAction: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRuleProfile(customID); err != nil {
		t.Fatal(err)
	}
	if profiles, err = st.ListRuleProfiles(); err != nil || len(profiles) != 1 || profiles[0].ID != id {
		t.Fatalf("delete custom rule profile: %v %+v", err, profiles)
	}
}
