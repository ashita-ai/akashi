package billing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewService_Enabled(t *testing.T) {
	svc := New(nil, Config{
		SecretKey:     "sk_test_xxx",
		WebhookSecret: "whsec_xxx",
		PriceIDPro:    "price_xxx",
	}, nil)

	assert.True(t, svc.Enabled())
}

func TestNewService_Disabled(t *testing.T) {
	svc := New(nil, Config{}, nil)

	assert.False(t, svc.Enabled())
}

func TestGetPlan(t *testing.T) {
	svc := New(nil, Config{PriceIDPro: "price_xxx"}, nil)

	tests := []struct {
		name     string
		planName string
		wantOK   bool
		wantPlan Plan
	}{
		{"free plan", "free", true, Plan{Name: "Free", DecisionLimit: 1_000, AgentLimit: 5}},
		{"pro plan", "pro", true, Plan{Name: "Pro", PriceID: "price_xxx", DecisionLimit: 50_000, AgentLimit: 0}},
		{"enterprise plan", "enterprise", true, Plan{Name: "Enterprise", DecisionLimit: 0, AgentLimit: 0}},
		{"unknown plan", "platinum", false, Plan{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, ok := svc.GetPlan(tt.planName)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.wantPlan.Name, plan.Name)
				assert.Equal(t, tt.wantPlan.DecisionLimit, plan.DecisionLimit)
				assert.Equal(t, tt.wantPlan.AgentLimit, plan.AgentLimit)
				assert.Equal(t, tt.wantPlan.PriceID, plan.PriceID)
			}
		})
	}
}

func TestCurrentPeriod(t *testing.T) {
	period := CurrentPeriod()
	require.NotEmpty(t, period)
	// Period should be in YYYY-MM format.
	assert.Regexp(t, `^\d{4}-\d{2}$`, period)
}

func TestCheckDecisionQuota_DisabledService(t *testing.T) {
	svc := New(nil, Config{}, nil)

	// Disabled service should always allow.
	err := svc.CheckDecisionQuota(nil, [16]byte{})
	assert.NoError(t, err)
}

func TestCheckAgentQuota_DisabledService(t *testing.T) {
	svc := New(nil, Config{}, nil)

	err := svc.CheckAgentQuota(nil, [16]byte{})
	assert.NoError(t, err)
}

func TestCreateCheckoutSession_Disabled(t *testing.T) {
	svc := New(nil, Config{}, nil)

	_, err := svc.CreateCheckoutSession(nil, "org-id", "test@example.com", "https://ok", "https://cancel")
	assert.ErrorIs(t, err, ErrBillingDisabled)
}

func TestCreatePortalSession_Disabled(t *testing.T) {
	svc := New(nil, Config{}, nil)

	_, err := svc.CreatePortalSession(nil, "cus_xxx", "https://return")
	assert.ErrorIs(t, err, ErrBillingDisabled)
}
