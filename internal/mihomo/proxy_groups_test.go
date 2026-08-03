package mihomo

import "testing"

func TestCandidateProxyGroupsShowSelectionsAndMissingTargets(t *testing.T) {
	groups, err := CandidateProxyGroups([]byte(`
proxies:
  - name: Tokyo
    type: ss
proxy-groups:
  - name: PROXY
    type: select
    proxies: [Tokyo, DIRECT, Missing]
  - name: AUTO
    type: url-test
    proxies: [Tokyo]
    use: [provider-a]
rules: [MATCH,PROXY]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || !groups[0].Main || !groups[0].Selectable || groups[0].Current != "Tokyo" || len(groups[0].Nodes) != 3 {
		t.Fatalf("groups=%#v", groups)
	}
	if groups[0].Nodes[2].Available || groups[0].Nodes[2].UnavailableReason == "" {
		t.Fatalf("missing node=%#v", groups[0].Nodes[2])
	}
	if groups[1].Selectable {
		t.Fatalf("automatic group selectable=%#v", groups[1])
	}
	if len(groups[1].Providers) != 1 || groups[1].Providers[0] != "provider-a" {
		t.Fatalf("provider-backed group=%#v", groups[1])
	}
}
