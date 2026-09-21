package main

import "strings"

// The default deployment accepts both personal (292) and Team (332) tickets.
// These are operator-selected length rules, not a cryptographic validation or
// a guarantee of upstream model behavior. Account/model isolation and expiry
// validation still apply to every stored value.
func isTemplateLength(length, templateLength int) bool {
	return length == templateLength || (templateLength == 292 && length == 332)
}

func isDefaultTicketLabel(label string) bool {
	return label == "292" || label == "332"
}

func isTeamPlan(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "team", "business":
		return true
	}
	return false
}

func isAccountTemplateLength(length, templateLength int, plan string) bool {
	if templateLength == 292 && isTeamPlan(plan) {
		return length == 332
	}
	return length == templateLength
}

func credentialPlanType(claims, blob map[string]any) string {
	// Prefer the access token's selected workspace, then the ID token. Never
	// infer a plan from a filename, email, display name or the returned length.
	for _, candidate := range []map[string]any{claims, probeJWTClaims(stringField(blob, "id_token")), blob} {
		if auth, ok := candidate["https://api.openai.com/auth"].(map[string]any); ok {
			if plan := strings.TrimSpace(stringField(auth, "chatgpt_plan_type")); plan != "" {
				return strings.ToLower(plan)
			}
		}
		for _, key := range []string{"chatgpt_plan_type", "plan_type"} {
			if plan := strings.TrimSpace(stringField(candidate, key)); plan != "" {
				return strings.ToLower(plan)
			}
		}
	}
	return ""
}

func accountPlanFromHost(account string) string {
	accounts, err := cachedCodexAuths()
	if err == nil {
		for _, candidate := range accounts {
			if candidate.AuthID == account {
				return candidate.PlanType
			}
		}
	}
	return ""
}
