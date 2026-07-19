package coopcmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCoopCommandDoesNotRegisterLegacyAgentCommands(t *testing.T) {
	cmd := newCoopCmd().cmd

	_, _, err := cmd.Find([]string{"step"})
	require.Error(t, err)

	_, _, err = cmd.Find([]string{"next-steps"})
	require.Error(t, err)
}

func TestCoopCommandRemovesPublicVerifyAndAddsReportWorkResourceFlag(t *testing.T) {
	cmd := newCoopCmd().cmd

	_, _, err := cmd.Find([]string{"verify"})
	require.Error(t, err)

	reportWork, _, err := cmd.Find([]string{"agent", "report-work"})
	require.NoError(t, err)
	require.NotNil(t, reportWork.Flags().Lookup("stripe-resource"))
}
