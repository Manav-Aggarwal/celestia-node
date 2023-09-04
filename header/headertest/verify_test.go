package headertest

import (
	"strconv"
	"testing"

	libhead "github.com/celestiaorg/go-header"
	"github.com/stretchr/testify/assert"
	tmrand "github.com/tendermint/tendermint/libs/rand"

	"github.com/celestiaorg/celestia-node/header"
)

func TestVerify(t *testing.T) {
	h := NewTestSuite(t, 2).GenExtendedHeaders(3)
	trusted, untrustedAdj, untrustedNonAdj := h[0], h[1], h[2]
	tests := []struct {
		prepare func() *header.ExtendedHeader
		err     error
	}{
		{
			prepare: func() *header.ExtendedHeader { return untrustedAdj },
			err:     nil,
		},
		{
			prepare: func() *header.ExtendedHeader {
				return untrustedNonAdj
			},
			err: nil,
		},
		{
			prepare: func() *header.ExtendedHeader {
				untrusted := *untrustedAdj
				untrusted.ValidatorsHash = tmrand.Bytes(32)
				return &untrusted
			},
			err: &libhead.VerifyError{
				Reason: header.ErrValidatorHashMismatch,
			},
		},
		{
			prepare: func() *header.ExtendedHeader {
				untrusted := *untrustedAdj
				untrusted.RawHeader.LastBlockID.Hash = tmrand.Bytes(32)
				return &untrusted
			},
			err: &libhead.VerifyError{
				Reason: header.ErrLastHeaderHashMismatch,
			},
		},
		{
			prepare: func() *header.ExtendedHeader {
				untrusted := *untrustedNonAdj
				untrusted.Commit = NewTestSuite(t, 2).Commit(RandRawHeader(t))
				return &untrusted
			},
			err: &libhead.VerifyError{
				Reason: header.ErrVerifyCommitLightTrustingFailed,
			},
		},
	}

	for i, test := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			err := trusted.Verify(test.prepare())
			if test.err == nil {
				assert.NoError(t, err)
				return
			}
			if err == nil {
				t.Errorf("expected err: %v, got nil", test.err)
				return
			}
			switch (err).(type) {
			case *libhead.VerifyError:
				reason := err.(*libhead.VerifyError).Reason
				testReason := test.err.(*libhead.VerifyError).Reason
				switch testReason {
				case header.ErrValidatorHashMismatch:
					assert.ErrorIs(t, reason, header.ErrValidatorHashMismatch)
				case header.ErrLastHeaderHashMismatch:
					assert.ErrorIs(t, reason, header.ErrLastHeaderHashMismatch)
				case header.ErrVerifyCommitLightTrustingFailed:
					assert.ErrorIs(t, reason, header.ErrVerifyCommitLightTrustingFailed)
				default:
					assert.Equal(t, testReason, reason)
				}
			default:
				assert.Failf(t, "unexpected error: %s\n", err.Error())
			}
		})
	}
}
