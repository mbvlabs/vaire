package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

type materialisationReport struct {
	Job           string `json:"owning_job,omitempty"`
	Phase         string `json:"phase"`
	Reason        string `json:"reason,omitempty"`
	PhysicalState string `json:"physical_state"`
	InRecovery    *bool  `json:"in_recovery,omitempty"`
	ReplayPaused  *bool  `json:"replay_paused,omitempty"`
}

func incompleteReportText(r *materialisationReport) string {
	if r == nil {
		return "application unavailable"
	}
	parts := []string{"application unavailable", "phase " + r.Phase, "physical " + r.PhysicalState}
	if r.Job != "" {
		parts = append(parts, "job "+r.Job)
	}
	if r.Reason != "" {
		parts = append(parts, "reason "+r.Reason)
	}
	if r.InRecovery != nil {
		parts = append(parts, fmt.Sprintf("in recovery %t", *r.InRecovery))
	}
	if r.ReplayPaused != nil {
		parts = append(parts, fmt.Sprintf("replay paused %t", *r.ReplayPaused))
	}
	return strings.Join(parts, ", ")
}

func omitIncompleteFields(raw []byte, keys ...string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, key := range keys {
		delete(fields, key)
	}
	return json.Marshal(fields)
}
