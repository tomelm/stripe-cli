package observe

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSupervisorConcurrentSnapshotsAndObservations(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), newScriptedConnector(connectStep{connection: connection}), clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)

	const observationCount = 100
	const drainReaders = 3
	deadline := time.Now().Add(2 * time.Second)
	collected := make([][]SequencedObservation, drainReaders)

	var group sync.WaitGroup
	for reader := 0; reader < drainReaders; reader++ {
		reader := reader
		group.Add(1)
		go func() {
			defer group.Done()
			var cursor uint64
			for len(collected[reader]) < observationCount && time.Now().Before(deadline) {
				observations, next, missed := supervisor.ObservationsSince(cursor)
				if missed != 0 {
					t.Errorf("reader %d missed %d observations", reader, missed)
					return
				}
				collected[reader] = append(collected[reader], observations...)
				cursor = next
				time.Sleep(time.Millisecond)
			}
		}()
	}
	for reader := 0; reader < 4; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := 0; iteration < 200; iteration++ {
				_ = supervisor.Snapshot()
			}
		}()
	}
	for index := 1; index <= observationCount; index++ {
		connection.observations <- Observation{Request: &RequestObservation{
			RequestID: fmt.Sprintf("req_%03d", index),
			Method:    "POST",
			Path:      "/v1/payment_intents",
			Status:    200,
		}}
	}
	group.Wait()

	// Every reader drains its own cursor from zero and must see every pushed
	// observation exactly once, in arrival order.
	for reader := 0; reader < drainReaders; reader++ {
		require.Len(t, collected[reader], observationCount, "reader %d", reader)
		for index, sequenced := range collected[reader] {
			require.EqualValues(t, index+1, sequenced.Sequence, "reader %d", reader)
			require.NotNil(t, sequenced.Observation.Request)
			require.Equal(t, fmt.Sprintf("req_%03d", index+1), sequenced.Observation.Request.RequestID)
		}
	}
	stopSupervisor(t, supervisor)
}
