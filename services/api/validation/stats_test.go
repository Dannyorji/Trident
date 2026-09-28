package validation

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateQueryStats_Defaults(t *testing.T) {
	params, err := ValidateQueryStats("", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assert.Equal(t, int64(50), params.Limit)
	assert.Nil(t, params.FromLedgerPtr)
	assert.Nil(t, params.ToLedgerPtr)
	// Network is deliberately left unset here — it is never parsed from the
	// query string (issue #612); the caller must populate it from
	// middleware.NetworkFromContext after validation succeeds.
	assert.Equal(t, "", params.Network)
}

func TestValidateQueryStats_ValidParams(t *testing.T) {
	params, err := ValidateQueryStats("1000", "5000", "100")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assert.Equal(t, int64(1000), params.FromLedger)
	assert.Equal(t, int64(5000), params.ToLedger)
	assert.Equal(t, int64(100), params.Limit)
}

func TestValidateQueryStats_InvalidFromLedger_NegativeNumber(t *testing.T) {
	_, err := ValidateQueryStats("-1", "", "")
	if assert.Error(t, err) {
		ve := err
		assert.Equal(t, "from_ledger", ve.Field)
	}
}

func TestValidateQueryStats_InvalidToLedger_NotInteger(t *testing.T) {
	_, err := ValidateQueryStats("", "abc", "")
	if assert.Error(t, err) {
		ve := err
		assert.Equal(t, "to_ledger", ve.Field)
	}
}

func TestValidateQueryStats_ToLedgerLessThanFromLedger(t *testing.T) {
	_, err := ValidateQueryStats("5000", "1000", "")
	if assert.Error(t, err) {
		ve := err
		assert.Equal(t, "to_ledger", ve.Field)
	}
}

func TestValidateQueryStats_InvalidLimit_TooSmall(t *testing.T) {
	_, err := ValidateQueryStats("", "", "0")
	if assert.Error(t, err) {
		ve := err
		assert.Equal(t, "limit", ve.Field)
	}
}

func TestValidateQueryStats_InvalidLimit_TooLarge(t *testing.T) {
	_, err := ValidateQueryStats("", "", "101")
	if assert.Error(t, err) {
		ve := err
		assert.Equal(t, "limit", ve.Field)
	}
}

func TestValidateQueryStats_InvalidLimit_NotInteger(t *testing.T) {
	_, err := ValidateQueryStats("", "", "abc")
	if assert.Error(t, err) {
		ve := err
		assert.Equal(t, "limit", ve.Field)
	}
}

func TestValidateQueryStats_LimitBoundary_Min(t *testing.T) {
	params, err := ValidateQueryStats("", "", "1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assert.Equal(t, int64(1), params.Limit)
}

func TestValidateQueryStats_LimitBoundary_Max(t *testing.T) {
	params, err := ValidateQueryStats("", "", "100")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assert.Equal(t, int64(100), params.Limit)
}

func TestValidateQueryStats_OneSidedRange_FromOnly_Rejected(t *testing.T) {
	_, err := ValidateQueryStats("1000", "", "")
	if assert.Error(t, err) {
		assert.Equal(t, "from_ledger", err.Field)
	}
}

func TestValidateQueryStats_OneSidedRange_ToOnly_Rejected(t *testing.T) {
	_, err := ValidateQueryStats("", "5000", "")
	if assert.Error(t, err) {
		assert.Equal(t, "from_ledger", err.Field)
	}
}

func TestValidateQueryStats_RangeAtCap_Accepted(t *testing.T) {
	from := int64(1000)
	to := from + StatsMaxLedgerRange
	params, err := ValidateQueryStats("1000", strconv.FormatInt(to, 10), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assert.Equal(t, to, params.ToLedger)
}

func TestValidateQueryStats_RangeOverCap_Rejected(t *testing.T) {
	from := int64(1000)
	to := from + StatsMaxLedgerRange + 1
	_, err := ValidateQueryStats("1000", strconv.FormatInt(to, 10), "")
	if assert.Error(t, err) {
		assert.Equal(t, "to_ledger", err.Field)
	}
}
