package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"cosmossdk.io/log"
	testutils "github.com/celestiaorg/utils/test"
	ds "github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	coreda "github.com/rollkit/rollkit/core/da"
	coreexecutor "github.com/rollkit/rollkit/core/execution"
	coresequencer "github.com/rollkit/rollkit/core/sequencer"
	rollkitconfig "github.com/rollkit/rollkit/pkg/config"
	"github.com/rollkit/rollkit/pkg/p2p"
	"github.com/rollkit/rollkit/pkg/p2p/key"
	remote_signer "github.com/rollkit/rollkit/pkg/signer/noop"
	"github.com/rollkit/rollkit/types"
)

// FullNodeTestSuite is a test suite for full node integration tests
type FullNodeTestSuite struct {
	suite.Suite
	ctx       context.Context
	cancel    context.CancelFunc
	node      *FullNode
	executor  *coreexecutor.DummyExecutor
	errCh     chan error
	runningWg sync.WaitGroup
}

// startNodeInBackground starts the given node in a background goroutine
// and adds to the wait group for proper cleanup
func (s *FullNodeTestSuite) startNodeInBackground(node *FullNode) {
	s.runningWg.Add(1)
	go func() {
		defer s.runningWg.Done()
		err := node.Run(s.ctx)
		select {
		case s.errCh <- err:
		default:
			s.T().Logf("Error channel full, discarding error: %v", err)
		}
	}()
}

func (s *FullNodeTestSuite) SetupTest() {
	require := require.New(s.T())
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.errCh = make(chan error, 1)

	// Setup node with proper configuration
	config := getTestConfig(s.T(), 1)
	config.Node.BlockTime.Duration = 100 * time.Millisecond // Faster block production for tests
	config.DA.BlockTime.Duration = 200 * time.Millisecond   // Faster DA submission for tests
	config.Node.MaxPendingBlocks = 100                      // Allow more pending blocks
	config.Node.Aggregator = true                           // Enable aggregator mode

	// Add debug logging for configuration
	s.T().Logf("Test configuration: BlockTime=%v, DABlockTime=%v, MaxPendingBlocks=%d",
		config.Node.BlockTime.Duration, config.DA.BlockTime.Duration, config.Node.MaxPendingBlocks)

	// Create genesis with current time
	genesis, genesisValidatorKey, _ := types.GetGenesisWithPrivkey("test-chain")
	remoteSigner, err := remote_signer.NewNoopSigner(genesisValidatorKey)
	require.NoError(err)

	// Create node key for P2P client
	nodeKey := &key.NodeKey{
		PrivKey: genesisValidatorKey,
		PubKey:  genesisValidatorKey.GetPublic(),
	}

	config.ChainID = genesis.ChainID

	dummyExec := coreexecutor.NewDummyExecutor()
	dummySequencer := coresequencer.NewDummySequencer()
	dummyDA := coreda.NewDummyDA(100_000, 0, 0)
	p2pClient, err := p2p.NewClient(config, nodeKey, dssync.MutexWrap(ds.NewMapDatastore()), log.NewTestLogger(s.T()), p2p.NopMetrics())
	require.NoError(err)

	err = InitFiles(config.RootDir)
	require.NoError(err)

	node, err := NewNode(
		s.ctx,
		config,
		dummyExec,
		dummySequencer,
		dummyDA,
		remoteSigner,
		*nodeKey,
		p2pClient,
		genesis,
		dssync.MutexWrap(ds.NewMapDatastore()),
		DefaultMetricsProvider(rollkitconfig.DefaultInstrumentationConfig()),
		log.NewTestLogger(s.T()),
	)
	require.NoError(err)
	require.NotNil(node)

	fn, ok := node.(*FullNode)
	require.True(ok)

	s.node = fn

	s.executor = dummyExec

	// Start the node in a goroutine using Run instead of Start
	s.startNodeInBackground(s.node)

	// Wait for the node to start and initialize DA connection
	time.Sleep(2 * time.Second)

	// Verify that the node is running and producing blocks
	height, err := getNodeHeight(s.node, Header)
	require.NoError(err, "Failed to get node height")
	require.Greater(height, uint64(0), "Node should have produced at least one block")

	// Wait for DA inclusion with retry
	err = testutils.Retry(30, 100*time.Millisecond, func() error {
		daHeight := s.node.blockManager.GetDAIncludedHeight()
		if daHeight == 0 {
			return fmt.Errorf("waiting for DA inclusion")
		}
		return nil
	})
	require.NoError(err, "Failed to get DA inclusion")

	// Wait for additional blocks to be produced
	time.Sleep(500 * time.Millisecond)

	// Additional debug info after node start
	initialHeight, err := s.node.Store.Height(s.ctx)
	require.NoError(err)
	s.T().Logf("Node started - Initial block height: %d", initialHeight)

	// Wait longer for height to stabilize and log intermediate values
	for range 5 {
		time.Sleep(200 * time.Millisecond)
		currentHeight, err := s.node.Store.Height(s.ctx)
		require.NoError(err)
		s.T().Logf("Current height during stabilization: %d", currentHeight)
	}

	// Get final height after stabilization period
	finalHeight, err := s.node.Store.Height(s.ctx)
	require.NoError(err)
	s.T().Logf("Final setup height: %d", finalHeight)

	// Store the stable height for test use
	s.node.blockManager.SetLastState(s.node.blockManager.GetLastState())

	// Log additional state information
	s.T().Logf("Last submitted height: %d", s.node.blockManager.PendingHeaders().GetLastSubmittedHeight())
	s.T().Logf("DA included height: %d", s.node.blockManager.GetDAIncludedHeight())

	// Verify sequencer client is working
	err = testutils.Retry(30, 100*time.Millisecond, func() error {
		if s.node.blockManager.SeqClient() == nil {
			return fmt.Errorf("sequencer client not initialized")
		}
		return nil
	})
	require.NoError(err, "Sequencer client initialization failed")

	// Verify block manager is properly initialized
	require.NotNil(s.node.blockManager, "Block manager should be initialized")
}

func (s *FullNodeTestSuite) TearDownTest() {
	if s.cancel != nil {
		s.cancel() // Cancel context to stop the node

		// Wait for the node to stop with a timeout
		waitCh := make(chan struct{})
		go func() {
			s.runningWg.Wait()
			close(waitCh)
		}()

		select {
		case <-waitCh:
			// Node stopped successfully
		case <-time.After(5 * time.Second):
			s.T().Log("Warning: Node did not stop gracefully within timeout")
		}

		// Check for any errors
		select {
		case err := <-s.errCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				s.T().Logf("Error stopping node in teardown: %v", err)
			}
		default:
			// No error
		}
	}
}

// TestFullNodeTestSuite runs the test suite
func TestFullNodeTestSuite(t *testing.T) {
	suite.Run(t, new(FullNodeTestSuite))
}

func (s *FullNodeTestSuite) TestSubmitBlocksToDA() {
	require := require.New(s.T())

	// Get initial state
	initialDAHeight := s.node.blockManager.GetDAIncludedHeight()
	initialHeight, err := getNodeHeight(s.node, Header)
	require.NoError(err)

	// Check if block manager is properly initialized
	s.T().Log("=== Block Manager State ===")
	pendingHeaders, err := s.node.blockManager.PendingHeaders().GetPendingHeaders()
	require.NoError(err)
	s.T().Logf("Initial Pending Headers: %d", len(pendingHeaders))
	s.T().Logf("Last Submitted Height: %d", s.node.blockManager.PendingHeaders().GetLastSubmittedHeight())

	// Verify sequencer is working
	s.T().Log("=== Sequencer Check ===")
	require.NotNil(s.node.blockManager.SeqClient(), "Sequencer client should be initialized")

	s.executor.InjectTx([]byte("dummy transaction"))

	// Monitor batch retrieval
	s.T().Log("=== Monitoring Batch Retrieval ===")
	for i := range 5 {
		time.Sleep(200 * time.Millisecond)
		// We can't directly check batch queue size, but we can monitor block production
		currentHeight, err := s.node.Store.Height(s.ctx)
		require.NoError(err)
		s.T().Logf("Current height after batch check %d: %d", i, currentHeight)
	}

	// Try to trigger block production explicitly
	s.T().Log("=== Attempting to Trigger Block Production ===")
	// Force a state update to trigger block production
	currentState := s.node.blockManager.GetLastState()
	currentState.LastBlockTime = time.Now().Add(-2 * s.node.nodeConfig.Node.BlockTime.Duration)
	s.node.blockManager.SetLastState(currentState)

	// Monitor after trigger
	for i := range 5 {
		time.Sleep(200 * time.Millisecond)
		currentHeight, err := s.node.Store.Height(s.ctx)
		require.NoError(err)
		currentDAHeight := s.node.blockManager.GetDAIncludedHeight()
		pendingHeaders, _ := s.node.blockManager.PendingHeaders().GetPendingHeaders()
		s.T().Logf("Post-trigger check %d - Height: %d, DA Height: %d, Pending: %d",
			i, currentHeight, currentDAHeight, len(pendingHeaders))
	}

	// Final assertions with more detailed error messages
	finalDAHeight := s.node.blockManager.GetDAIncludedHeight()
	finalHeight, err := s.node.Store.Height(s.ctx)
	require.NoError(err)

	require.Greater(finalHeight, initialHeight, "Block height should have increased")
	require.Greater(finalDAHeight, initialDAHeight, "DA height should have increased")
}

func (s *FullNodeTestSuite) TestDAInclusion() {
	require := require.New(s.T())

	// Get initial height and DA height
	initialHeight, err := getNodeHeight(s.node, Header)
	require.NoError(err, "Failed to get initial height")
	initialDAHeight := s.node.blockManager.GetDAIncludedHeight()

	s.T().Logf("=== Initial State ===")
	s.T().Logf("Block height: %d, DA height: %d", initialHeight, initialDAHeight)
	s.T().Logf("Aggregator enabled: %v", s.node.nodeConfig.Node.Aggregator)

	s.executor.InjectTx([]byte("dummy transaction"))

	// Monitor state changes in shorter intervals
	s.T().Log("=== Monitoring State Changes ===")
	for i := range 10 {
		time.Sleep(200 * time.Millisecond)
		currentHeight, err := s.node.Store.Height(s.ctx)
		require.NoError(err)
		currentDAHeight := s.node.blockManager.GetDAIncludedHeight()
		pendingHeaders, _ := s.node.blockManager.PendingHeaders().GetPendingHeaders()
		lastSubmittedHeight := s.node.blockManager.PendingHeaders().GetLastSubmittedHeight()

		s.T().Logf("Iteration %d:", i)
		s.T().Logf("  - Height: %d", currentHeight)
		s.T().Logf("  - DA Height: %d", currentDAHeight)
		s.T().Logf("  - Pending Headers: %d", len(pendingHeaders))
		s.T().Logf("  - Last Submitted Height: %d", lastSubmittedHeight)
	}

	s.T().Log("=== Checking DA Height Increase ===")
	// Use shorter retry period with more frequent checks
	var finalDAHeight uint64
	err = testutils.Retry(30, 200*time.Millisecond, func() error {
		currentDAHeight := s.node.blockManager.GetDAIncludedHeight()
		currentHeight, err := s.node.Store.Height(s.ctx)
		require.NoError(err)
		pendingHeaders, _ := s.node.blockManager.PendingHeaders().GetPendingHeaders()

		s.T().Logf("Retry check - DA Height: %d, Block Height: %d, Pending: %d",
			currentDAHeight, currentHeight, len(pendingHeaders))

		if currentDAHeight <= initialDAHeight {
			return fmt.Errorf("waiting for DA height to increase from %d (current: %d)",
				initialDAHeight, currentDAHeight)
		}
		finalDAHeight = currentDAHeight
		return nil
	})
	require.NoError(err, "DA height did not increase")

	// Final state logging
	s.T().Log("=== Final State ===")
	finalHeight, err := s.node.Store.Height(s.ctx)
	require.NoError(err)
	pendingHeaders, _ := s.node.blockManager.PendingHeaders().GetPendingHeaders()
	s.T().Logf("Final Height: %d", finalHeight)
	s.T().Logf("Final DA Height: %d", finalDAHeight)
	s.T().Logf("Final Pending Headers: %d", len(pendingHeaders))

	// Assertions
	require.NoError(err, "DA height did not increase")
	require.Greater(finalHeight, initialHeight, "Block height should increase")
	require.Greater(finalDAHeight, initialDAHeight, "DA height should increase")
}

func (s *FullNodeTestSuite) TestMaxPending() {
	require := require.New(s.T())

	// First, stop the current node by cancelling its context
	s.cancel()

	// Create a new context for the new node
	s.ctx, s.cancel = context.WithCancel(context.Background())

	// Reset error channel
	s.errCh = make(chan error, 1)

	// Reconfigure node with low max pending
	config := getTestConfig(s.T(), 1)
	config.Node.MaxPendingBlocks = 2

	genesis, genesisValidatorKey, _ := types.GetGenesisWithPrivkey("test-chain")
	remoteSigner, err := remote_signer.NewNoopSigner(genesisValidatorKey)
	require.NoError(err)

	nodeKey, err := key.GenerateNodeKey()
	require.NoError(err)

	executor, sequencer, dac, p2pClient, ds := createTestComponents(s.T())

	err = InitFiles(config.RootDir)
	require.NoError(err)

	node, err := NewNode(
		s.ctx,
		config,
		executor,
		sequencer,
		dac,
		remoteSigner,
		*nodeKey,
		p2pClient,
		genesis,
		ds,
		DefaultMetricsProvider(rollkitconfig.DefaultInstrumentationConfig()),
		log.NewTestLogger(s.T()),
	)
	require.NoError(err)
	require.NotNil(node)

	fn, ok := node.(*FullNode)
	require.True(ok)

	s.node = fn

	// Start the node using Run in a goroutine
	s.startNodeInBackground(s.node)

	// Wait blocks to be produced up to max pending
	time.Sleep(time.Duration(config.Node.MaxPendingBlocks+1) * config.Node.BlockTime.Duration)

	// Verify that number of pending blocks doesn't exceed max
	height, err := getNodeHeight(s.node, Header)
	require.NoError(err)
	require.LessOrEqual(height, config.Node.MaxPendingBlocks)
}

func (s *FullNodeTestSuite) TestGenesisInitialization() {
	require := require.New(s.T())

	// Verify genesis state
	state := s.node.blockManager.GetLastState()
	require.Equal(s.node.genesis.InitialHeight, state.InitialHeight)
	require.Equal(s.node.genesis.ChainID, state.ChainID)
}

func (s *FullNodeTestSuite) TestStateRecovery() {
	s.T().Skip("skipping state recovery test, we need to reuse the same database, when we use in memory it starts fresh each time")
	require := require.New(s.T())

	// Get current state
	originalHeight, err := getNodeHeight(s.node, Store)
	require.NoError(err)

	// Wait for some blocks
	time.Sleep(2 * s.node.nodeConfig.Node.BlockTime.Duration)

	// Stop the current node
	s.cancel()

	// Wait for the node to stop
	waitCh := make(chan struct{})
	go func() {
		s.runningWg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		// Node stopped successfully
	case <-time.After(2 * time.Second):
		s.T().Log("Warning: Node did not stop gracefully within timeout")
	}

	// Create a new context
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.errCh = make(chan error, 1)

	// Create a NEW node instance instead of reusing the old one
	config := getTestConfig(s.T(), 1)
	genesis, genesisValidatorKey, _ := types.GetGenesisWithPrivkey("test-chain")
	remoteSigner, err := remote_signer.NewNoopSigner(genesisValidatorKey)
	require.NoError(err)

	dummyExec := coreexecutor.NewDummyExecutor()
	dummySequencer := coresequencer.NewDummySequencer()
	dummyDA := coreda.NewDummyDA(100_000, 0, 0)

	config.ChainID = genesis.ChainID
	p2pClient, err := p2p.NewClient(config, nil, dssync.MutexWrap(ds.NewMapDatastore()), log.NewTestLogger(s.T()), p2p.NopMetrics())
	require.NoError(err)

	nodeKey, err := key.GenerateNodeKey()
	require.NoError(err)

	node, err := NewNode(
		s.ctx,
		config,
		dummyExec,
		dummySequencer,
		dummyDA,
		remoteSigner,
		*nodeKey,
		p2pClient,
		genesis,
		dssync.MutexWrap(ds.NewMapDatastore()),
		DefaultMetricsProvider(rollkitconfig.DefaultInstrumentationConfig()),
		log.NewTestLogger(s.T()),
	)
	require.NoError(err)

	fn, ok := node.(*FullNode)
	require.True(ok)

	// Replace the old node with the new one
	s.node = fn

	// Start the new node
	s.startNodeInBackground(s.node)

	// Wait a bit after restart
	time.Sleep(s.node.nodeConfig.Node.BlockTime.Duration)

	// Verify state persistence
	recoveredHeight, err := getNodeHeight(s.node, Store)
	require.NoError(err)
	require.GreaterOrEqual(recoveredHeight, originalHeight)
}
