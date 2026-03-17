package types

import "time"

// Severity represents the urgency level of a monitoring alert.
type Severity int

const (
	SeverityNone      Severity = iota
	SeverityInfo               // ℹ️  Status update, no action needed
	SeverityWarning            // ⚠️  Attention needed soon
	SeverityCritical           // 🔴  Imminent failure likely
	SeverityEmergency          // 🚨  System broken, immediate action required
	SeverityResolved           // ✅  Issue cleared
)

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "INFO"
	case SeverityWarning:
		return "WARNING"
	case SeverityCritical:
		return "CRITICAL"
	case SeverityEmergency:
		return "EMERGENCY"
	case SeverityResolved:
		return "RESOLVED"
	default:
		return "NONE"
	}
}

func (s Severity) Emoji() string {
	switch s {
	case SeverityInfo:
		return "ℹ️"
	case SeverityWarning:
		return "⚠️"
	case SeverityCritical:
		return "🔴"
	case SeverityEmergency:
		return "🚨"
	case SeverityResolved:
		return "✅"
	default:
		return ""
	}
}

// Alert is the unit of work passed to the alerting layer.
type Alert struct {
	Key      string   // unique key used for deduplication / cooldown tracking
	Severity Severity
	Title    string
	Body     string
	Time     time.Time
}

// --- Oracle price types ---

type OraclePriceResponse struct {
	Prices []OraclePrice `json:"prices"`
}

type OraclePrice struct {
	ID    OraclePriceID    `json:"id"`
	State OraclePriceState `json:"state"`
}

type OraclePriceID struct {
	Denom     string `json:"denom"`
	BaseDenom string `json:"base_denom"`
	Source    int    `json:"source"`
	Height    string `json:"height"`
}

type OraclePriceState struct {
	Price     string `json:"price"`
	Timestamp string `json:"timestamp"`
}

// --- Hermes health types ---

// HermesHealthResponse is the JSON body returned by the Hermes /health endpoint.
type HermesHealthResponse struct {
	IsRunning       bool   `json:"isRunning"`
	Address         string `json:"address"`
	PriceFeedID     string `json:"priceFeedId"`
	ContractAddress string `json:"contractAddress"`
}

// --- Wallet balance types ---

type WalletBalanceResponse struct {
	Balances []Balance `json:"balances"`
}

type Balance struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}
