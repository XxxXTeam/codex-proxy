package auth

import (
	"encoding/json"
	"testing"
)

func TestAccountFromTokenFilePreservesStoredPlanType(t *testing.T) {
	account, err := accountFromTokenFile(&TokenFile{
		RefreshToken: "refresh-token",
		PlanType:     "plus",
	}, "account.json")
	if err != nil {
		t.Fatalf("accountFromTokenFile() error = %v", err)
	}
	if got := account.Token.PlanType; got != "plus" {
		t.Fatalf("plan type = %q, want plus", got)
	}
}

func TestUpdateTokenPreservesPlanTypeWhenRefreshOmitsIt(t *testing.T) {
	account := &Account{Token: TokenData{PlanType: "team"}}
	account.UpdateToken(TokenData{AccessToken: "access-token"})

	if got := account.TokenSnapshot().PlanType; got != "team" {
		t.Fatalf("plan type = %q, want team", got)
	}
}

func TestRefreshUsedPercentReadsPrimaryWindow(t *testing.T) {
	rawData, err := json.Marshal(map[string]any{
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": 48,
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	account := &Account{
		QuotaInfo: &QuotaInfo{
			Valid:   true,
			RawData: rawData,
		},
	}
	account.RefreshUsedPercent()

	if got := account.GetUsedPercent(); got != 48 {
		t.Fatalf("used percent = %v, want 48", got)
	}
}
