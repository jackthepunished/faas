package preflight

import "github.com/onebox-faas/faas/pkg/api"

// PlanBudget is what one plan buys, stated as running time rather than as a
// recommendation.
//
// Preflight deliberately does not estimate how much memory an app needs:
// static analysis cannot see a working set, and a plausible-looking guess is
// exactly the kind of invented number this tool exists to avoid. So the report
// shows what each tier includes and leaves the choice to someone who knows
// their app.
type PlanBudget struct {
	Plan        api.Plan `json:"plan"`
	RAMMB       int      `json:"ram_mb"`
	BilledRAMMB int      `json:"billed_ram_mb"`
	// IncludedRunningMinutes is the allowance expressed as wall-clock running
	// time at this plan's billed RAM. Billing counts running seconds only, so
	// a parked app consumes none of it.
	IncludedRunningMinutes int   `json:"included_running_minutes"`
	IncludedGBHours        int   `json:"included_gb_hours"`
	PriceMillicents        int64 `json:"price_millicents"`
	// OverageMillicentsPerGBHour applies past the included allowance.
	OverageMillicentsPerGBHour int64 `json:"overage_millicents_per_gb_hour"`
}

// planOrder is the customer-facing ladder, cheapest first.
var planOrder = []api.Plan{api.PlanFree, api.PlanHobby, api.PlanPro, api.PlanScale}

// PlanBudgets returns every plan's allowance converted to running time. All
// values derive from pkg/api/limits.go; nothing here is a literal.
func PlanBudgets() []PlanBudget {
	budgets := make([]PlanBudget, 0, len(planOrder))
	for _, plan := range planOrder {
		limits, ok := api.LimitsFor(plan)
		if !ok {
			continue
		}
		billed := api.BillableRAMMB(limits.RAMMB)
		budgets = append(budgets, PlanBudget{
			Plan:        plan,
			RAMMB:       limits.RAMMB,
			BilledRAMMB: billed,
			// Integer arithmetic throughout: minutes = GB-hours × MB-per-GB ×
			// minutes-per-hour ÷ billed MB. No floats near a billing figure.
			IncludedRunningMinutes:     includedRunningMinutes(limits.IncludedGBHours, billed),
			IncludedGBHours:            limits.IncludedGBHours,
			PriceMillicents:            limits.PriceMillicents,
			OverageMillicentsPerGBHour: api.OverageMillicentsPerGBHour,
		})
	}
	return budgets
}

func includedRunningMinutes(includedGBHours, billedMB int) int {
	if billedMB <= 0 {
		return 0
	}
	return includedGBHours * 1024 * 60 / billedMB
}
