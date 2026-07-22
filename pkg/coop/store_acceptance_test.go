package coop

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreAcceptanceSerializesConcurrentUpdatesWithoutLostWrites(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &Session{
		ID:     "concurrent_acceptance",
		Status: SessionActive,
		Params: map[string]string{"writes": "0"},
	}
	require.NoError(t, store.Write(session))

	const (
		workers         = 6
		writesPerWorker = 12
	)
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for write := 0; write < writesPerWorker; write++ {
				if _, updateErr := store.Update(session.ID, func(current *Session) error {
					count, parseErr := strconv.Atoi(current.Params["writes"])
					if parseErr != nil {
						return parseErr
					}
					current.Params["writes"] = strconv.Itoa(count + 1)
					return nil
				}); updateErr != nil {
					errors <- updateErr
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errors)
	for updateErr := range errors {
		require.NoError(t, updateErr)
	}

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(workers*writesPerWorker), loaded.Params["writes"])
	assert.Equal(t, 1+workers*writesPerWorker, loaded.Version)
}

func TestStoreAcceptancePinsFirstStripeAccountAtomically(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &Session{ID: "pin_acceptance", Status: SessionActive}
	require.NoError(t, store.Write(session))

	start := make(chan struct{})
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for _, accountID := range []string{"acct_first", "acct_second"} {
		wait.Add(1)
		go func(accountID string) {
			defer wait.Done()
			<-start
			_, pinErr := store.PinStripeAccount(session.ID, accountID)
			errors <- pinErr
		}(accountID)
	}
	close(start)
	wait.Wait()
	close(errors)
	for pinErr := range errors {
		require.NoError(t, pinErr)
	}

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	assert.Contains(t, []string{"acct_first", "acct_second"}, loaded.StripeAccountID)
	assert.Equal(t, 2, loaded.Version, "only the winning pin should write the session")
	same, err := store.PinStripeAccount(session.ID, loaded.StripeAccountID)
	require.NoError(t, err)
	assert.Equal(t, loaded.StripeAccountID, same.StripeAccountID)
	assert.Equal(t, loaded.Version, same.Version, "repeating the winning pin must be a no-op")

	losingAccount := "acct_first"
	if loaded.StripeAccountID == losingAccount {
		losingAccount = "acct_second"
	}
	unchanged, err := store.PinStripeAccount(session.ID, losingAccount)
	require.NoError(t, err)
	assert.Equal(t, loaded.StripeAccountID, unchanged.StripeAccountID)
	assert.Equal(t, loaded.Version, unchanged.Version)
}
