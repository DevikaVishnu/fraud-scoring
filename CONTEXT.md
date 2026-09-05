# Fraud Detection Pipeline

Real-time risk scoring for card authorization attempts. An authorization arrives, a risk score is computed from historical and in-flight signals, and a decision is returned to the caller before the card network's deadline expires.

## Language

**Authorization**:
A request from the card network to approve or decline a card payment, held open by the network under a hard deadline. The unit of work this system scores.
_Avoid_: Transaction, payment, charge

**Signal**:
One piece of information about an authorization that the model reads when scoring it. Either taken straight from the authorization itself, or derived from the card's history.
_Avoid_: Feature, attribute, variable

**Event Time**:
The timestamp carried on the authorization itself. Every window and every gap in this system is measured against it, never against the clock time at which scoring happens.
_Avoid_: Processing time, wall clock, ingestion time

**Velocity Counter**:
A count of payments made on one card within a time window, measured in event time. Maintained in the background and permitted to lag, because the windows in use are long enough that lag is immaterial.
_Avoid_: Aggregate, rolling count, frequency feature

**Recency Check**:
The gap between an authorization and the previous payment on the same card, measured against the ordering of event time then transaction id. One of the signals the model reads; it feeds no rule of its own.
_Avoid_: Live signal, real-time feature

**Risk Score**:
The model's estimate of the probability that an authorization is fraud. Calibrated, so that a score of 0.3 means fraud roughly three times in ten; the expected cost arithmetic is meaningless otherwise.
_Avoid_: Fraud score, rating, confidence

**Expected Cost**:
The money a decision is predicted to lose on one authorization, given the risk score and the payment amount. Each of the three decisions has one, and the cheapest wins. There are no thresholds.
_Avoid_: Loss function, utility, threshold

**Decision**:
The verdict this system returns for an authorization: approve, decline, or challenge. The system returns it; it does not carry it out.
_Avoid_: Action, verdict, outcome

**Challenge**:
A decision that asks the cardholder's bank to verify the cardholder's identity before the payment proceeds. If they pass, the bank carries the loss for any later fraud on that payment.
_Avoid_: Step-up, 3DS, friction
