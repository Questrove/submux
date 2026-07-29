package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"submux/internal/store"
)

type resourceDeletionResponse struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error"`
	ResourceKind string `json:"resource_kind"`
	ResourceID   int64  `json:"resource_id"`
	Reason       string `json:"reason"`
	References   []struct {
		SubscriptionID   int64  `json:"subscription_id"`
		SubscriptionName string `json:"subscription_name"`
	} `json:"references"`
}

func TestResourceDeletionHandlersReturnStructuredConflicts(t *testing.T) {
	st := newTestStore(t)
	sourceID, err := st.CreateSource(store.Source{Name: "manual", Kind: store.SourceKindManual})
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := st.CreateManualNode(store.NodeRecord{
		SourceID: sourceID, Name: "node", Fingerprint: "node",
	})
	if err != nil {
		t.Fatal(err)
	}
	templateID, err := st.SaveTemplate(store.Template{
		Name: "template", Engine: "mihomo", Scenario: "desktop", Status: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.PublishTemplateVersion(templateID, "", "proxies: []", nil)
	if err != nil {
		t.Fatal(err)
	}
	ruleProfileID, err := st.SaveRuleProfile(store.RuleProfile{
		Name: "rules", FallbackAction: "proxy",
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriptionID, err := st.SaveOutputSubscription(store.OutputSubscription{
		Name:              "consumer",
		Token:             "consumer",
		Enabled:           false,
		TemplateVersionID: version.ID,
		RuleProfileID:     ruleProfileID,
		Bindings: []store.SubscriptionBinding{{
			Slot: "primary", NodeIDs: []int64{nodeID},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	client := initAndClient(t, srv)
	tests := []struct {
		name         string
		path         string
		resourceKind string
		resourceID   int64
	}{
		{"source", "/api/sources/" + formatResourceID(sourceID), "source", sourceID},
		{"node", "/api/nodes/" + formatResourceID(nodeID), "node", nodeID},
		{"template", "/api/templates/" + formatResourceID(templateID), "template", templateID},
		{"rule profile", "/api/rule-profiles/" + formatResourceID(ruleProfileID), "rule_profile", ruleProfileID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodDelete, srv.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response := mustDo(t, client, request)
			defer response.Body.Close()
			if response.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusConflict)
			}
			if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("content type = %q", contentType)
			}
			var body resourceDeletionResponse
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.OK || body.ResourceKind != test.resourceKind || body.ResourceID != test.resourceID || body.Reason != "referenced" {
				t.Fatalf("body = %+v", body)
			}
			if len(body.References) != 1 ||
				body.References[0].SubscriptionID != subscriptionID ||
				body.References[0].SubscriptionName != "consumer" {
				t.Fatalf("references = %+v", body.References)
			}
		})
	}
}

func TestResourceDeletionHandlersMapMissingResourcesToNotFound(t *testing.T) {
	st := newTestStore(t)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	client := initAndClient(t, srv)
	for _, path := range []string{
		"/api/sources/999",
		"/api/nodes/999",
		"/api/templates/999",
		"/api/rule-profiles/999",
	} {
		t.Run(path, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response := mustDo(t, client, request)
			defer response.Body.Close()
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
			}
			var body resourceDeletionResponse
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.OK || body.Error == "" {
				t.Fatalf("body = %+v", body)
			}
		})
	}
}

func TestResourceDeletionErrorMapsUnexpectedFailuresToInternalServerError(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeResourceDeletionError(recorder, errors.New("storage is corrupt"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	var body resourceDeletionResponse
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Error != "resource deletion failed" {
		t.Fatalf("body = %+v", body)
	}
}

func formatResourceID(value int64) string {
	return strconv.FormatInt(value, 10)
}
