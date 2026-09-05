// Package decisioning picks a decision by comparing what each one is expected to
// cost.
//
// There are no approve/decline thresholds here and there must not be: the
// decision rule is the arithmetic and nothing else. Because the payment amount
// sits inside the cost of a missed fraud, the cutoff between decisions moves
// with the amount on its own, with nothing tuned per amount band (ADR-0003).
package decisioning

import (
	"encoding/json"
	"fmt"
	"os"
)

// A Verdict is what the system returns for an authorization. It returns it; it
// does not carry it out.
type Verdict string

const (
	Approve   Verdict = "approve"
	Decline   Verdict = "decline"
	Challenge Verdict = "challenge"
)

// order is the preference among equally cheap verdicts, least intervention
// first, so a tie is resolved the same way every time.
var order = []Verdict{Approve, Challenge, Decline}

// Costs are the chosen inputs to the arithmetic. They are configuration rather
// than constants because they are a judgement, not a measurement.
type Costs struct {
	// A missed fraud costs the amount back, plus the fixed cost of handling it.
	FraudLossMultiple float64 `json:"fraud_loss_multiple"`
	ChargebackFee     float64 `json:"chargeback_fee"`

	// Turning away a legitimate cardholder costs goodwill plus lost margin.
	FalseDeclineFixed float64 `json:"false_decline_fixed"`
	FalseDeclineRate  float64 `json:"false_decline_rate"`

	// Every challenge costs friction; some legitimate cardholders abandon, and
	// abandonment costs what a false decline costs.
	ChallengeCost        float64 `json:"challenge_cost"`
	ChallengeAbandonRate float64 `json:"challenge_abandon_rate"`

	// The risk score to use when scoring fails, so degradation needs no second
	// policy: the same formula, with the prior in place of a score.
	BaseFraudRate float64 `json:"base_fraud_rate"`
}

func LoadCosts(path string) (Costs, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Costs{}, fmt.Errorf("loading costs %s: %w", path, err)
	}
	var costs Costs
	if err := json.Unmarshal(raw, &costs); err != nil {
		return Costs{}, fmt.Errorf("parsing costs %s: %w", path, err)
	}
	if costs.BaseFraudRate <= 0 || costs.BaseFraudRate >= 1 {
		return Costs{}, fmt.Errorf("costs %s: base fraud rate %v is not a probability",
			path, costs.BaseFraudRate)
	}
	return costs, nil
}

// ExpectedCosts is what each decision is predicted to lose on one
// authorization, in dollars, given the risk score and the payment amount.
func (c Costs) ExpectedCosts(riskScore, amountUSD float64) map[Verdict]float64 {
	fraud, legitimate := riskScore, 1-riskScore
	falseDecline := c.FalseDeclineFixed + c.FalseDeclineRate*amountUSD

	return map[Verdict]float64{
		// Approving hands the money to a fraudster, when it is one.
		Approve: fraud * (amountUSD*c.FraudLossMultiple + c.ChargebackFee),

		// Declining turns away a cardholder, when they are genuine. The cost of
		// doing that is counted rather than assumed to be zero.
		Decline: legitimate * falseDecline,

		// A challenge costs friction on every authorization it is used on, and
		// costs a false decline whenever a legitimate cardholder gives up. A
		// fraudster who passes one moves the liability to the bank, so the
		// fraud branch contributes nothing.
		Challenge: c.ChallengeCost + legitimate*c.ChallengeAbandonRate*falseDecline,
	}
}

// Cheapest is the decision. Nothing else is.
func (c Costs) Cheapest(riskScore, amountUSD float64) (Verdict, map[Verdict]float64) {
	costs := c.ExpectedCosts(riskScore, amountUSD)
	cheapest := order[0]
	for _, verdict := range order[1:] {
		if costs[verdict] < costs[cheapest] {
			cheapest = verdict
		}
	}
	return cheapest, costs
}
