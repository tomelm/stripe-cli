package observe

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func TestSupervisorConcurrentSnapshotsAndObservations(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), newScriptedConnector(connectStep{connection: connection}), clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)

	var group sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		group.Add(1)
		go func(reader int) {
			defer group.Done()
			for iteration := 0; iteration < 100; iteration++ {
				_ = supervisor.Snapshot().Summary()
				_ = supervisor.HealthEpochs()
				_, _ = supervisor.AvailabilityResult(
					verificationResultID(reader, iteration),
					"collector.logs",
				)
			}
		}(reader)
	}
	for index := 0; index < 100; index++ {
		connection.observations <- Observation{Request: &RequestObservation{
			RequestID: fmt.Sprintf("req_%d", index),
			Method:    "POST",
			Path:      "/v1/payment_intents",
			Status:    200,
		}}
	}
	group.Wait()
	require.Eventually(t, func() bool {
		return supervisor.Snapshot().ObservedRequests == 100
	}, 2*time.Second, time.Millisecond)
	stopSupervisor(t, supervisor)
}

func verificationResultID(reader, iteration int) verification.ResultID {
	return verification.ResultID(fmt.Sprintf("collector.logs:r%d-i%d", reader, iteration))
}
