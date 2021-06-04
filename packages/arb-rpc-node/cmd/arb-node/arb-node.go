/*
 * Copyright 2020, Offchain Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	golog "log"
	"math/big"
	"net/http"
	_ "net/http/pprof"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"

	"github.com/pkg/errors"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/rs/zerolog/pkgerrors"

	"github.com/offchainlabs/arbitrum/packages/arb-node-core/cmdhelp"
	"github.com/offchainlabs/arbitrum/packages/arb-node-core/ethbridge"
	"github.com/offchainlabs/arbitrum/packages/arb-node-core/metrics"
	"github.com/offchainlabs/arbitrum/packages/arb-node-core/monitor"
	"github.com/offchainlabs/arbitrum/packages/arb-node-core/nodehealth"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/aggregator"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/batcher"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/rpc"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/txdb"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/web3"
	"github.com/offchainlabs/arbitrum/packages/arb-util/broadcastclient"
	"github.com/offchainlabs/arbitrum/packages/arb-util/broadcaster"
	"github.com/offchainlabs/arbitrum/packages/arb-util/common"
	"github.com/offchainlabs/arbitrum/packages/arb-util/configuration"
)

var logger zerolog.Logger

var pprofMux *http.ServeMux

const largeChannelBuffer = 200

func init() {
	pprofMux = http.DefaultServeMux
	http.DefaultServeMux = http.NewServeMux()
}

func main() {
	// Enable line numbers in logging
	golog.SetFlags(golog.LstdFlags | golog.Lshortfile)

	// Print stack trace when `.Error().Stack().Err(err).` is added to zerolog call
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack

	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	// Print line number that log was created on
	logger = log.With().Caller().Stack().Str("component", "arb-node").Logger()

	if err := startup(); err != nil {
		logger.Error().Err(err).Msg("Error running node")
	}
}

func printSampleUsage() {
	fmt.Printf("\n")
	fmt.Printf("Sample usage:                  arb-node --conf=<filename> \n")
	fmt.Printf("          or:  forwarder node: arb-node --l1.url=<L1 RPC> [optional arguments]\n\n")
	fmt.Printf("          or: aggregator node: arb-node --l1.url=<L1 RPC> --node.type=aggregator [optional arguments] %s\n", cmdhelp.WalletArgsString)
	fmt.Printf("          or:       sequencer: arb-node --l1.url=<L1 RPC> --node.type=sequencer [optional arguments] %s\n", cmdhelp.WalletArgsString)
}

func startup() error {
	ctx, cancelFunc, cancelChan := cmdhelp.CreateLaunchContext()
	defer cancelFunc()

	config, wallet, l1Client, l1ChainId, err := configuration.ParseNode(ctx)
	if err != nil || len(config.Persistent.GlobalConfig) == 0 || len(config.L1.URL) == 0 ||
		len(config.Rollup.Address) == 0 || len(config.BridgeUtilsAddress) == 0 ||
		((config.Node.Type != "sequencer") && len(config.Node.Sequencer.Lockout.Redis) != 0) ||
		((len(config.Node.Sequencer.Lockout.Redis) == 0) != (len(config.Node.Sequencer.Lockout.SelfRPCURL) == 0)) {
		printSampleUsage()
		if err != nil && !strings.Contains(err.Error(), "help requested") {
			fmt.Printf("%s\n", err.Error())
		}

		return nil
	}

	badConfig := false
	if config.BridgeUtilsAddress == "" {
		badConfig = true
		fmt.Println("Missing --bridge-utils-address")
	}
	if config.Persistent.Chain == "" {
		badConfig = true
		fmt.Println("Missing --persistent.chain")
	}
	if config.Rollup.Address == "" {
		badConfig = true
		fmt.Println("Missing --rollup.address")
	}
	if config.Node.ChainID == 0 {
		badConfig = true
		fmt.Println("Missing --rollup.chain-id")
	}
	if config.Rollup.Machine.Filename == "" {
		badConfig = true
		fmt.Println("Missing --rollup.machine.filename")
	}

	if config.Node.Type == "forwarder" {
		if config.Node.Forwarder.Target == "" {
			badConfig = true
			fmt.Println("Forwarder node needs --node.forwarder.target")
		}
	} else if config.Node.Type == "aggregator" {
		if config.Node.Aggregator.InboxAddress == "" {
			badConfig = true
			fmt.Println("Aggregator node needs --node.aggregator.inbox-address")
		}
	} else if config.Node.Type == "sequencer" {
		// Sequencer always waits
		config.WaitToCatchUp = true
	} else {
		badConfig = true
		fmt.Printf("Unrecognized node type %s", config.Node.Type)
	}

	if badConfig {
		return nil
	}

	defer logger.Log().Msg("Cleanly shutting down node")

	if err := cmdhelp.ParseLogFlags(&config.Log.RPC, &config.Log.Core); err != nil {
		return err
	}

	if config.PProfEnable {
		go func() {
			err := http.ListenAndServe("localhost:8081", pprofMux)
			log.Error().Err(err).Msg("profiling server failed")
		}()
	}

	l2ChainId := new(big.Int).SetUint64(config.Node.ChainID)
	rollupAddress := common.HexToAddress(config.Rollup.Address)
	logger.Info().Hex("chainaddress", rollupAddress.Bytes()).Hex("chainid", l2ChainId.Bytes()).Str("type", config.Node.Type).Msg("Launching arbitrum node")

	mon, err := monitor.NewMonitor(config.GetNodeDatabasePath(), config.Rollup.Machine.Filename)
	if err != nil {
		return errors.Wrap(err, "error opening monitor")
	}
	defer mon.Close()

	metricsConfig := metrics.NewMetricsConfig(&config.Healthcheck.MetricsPrefix)
	healthChan := make(chan nodehealth.Log, largeChannelBuffer)
	go func() {
		err := nodehealth.StartNodeHealthCheck(ctx, healthChan, metricsConfig.Registry, metricsConfig.Registerer)
		if err != nil {
			log.Error().Err(err).Msg("healthcheck server failed")
		}
	}()

	healthChan <- nodehealth.Log{Config: true, Var: "healthcheckMetrics", ValBool: config.Healthcheck.Metrics}
	healthChan <- nodehealth.Log{Config: true, Var: "disablePrimaryCheck", ValBool: !config.Healthcheck.Sequencer}
	healthChan <- nodehealth.Log{Config: true, Var: "disableOpenEthereumCheck", ValBool: !config.Healthcheck.L1Node}
	healthChan <- nodehealth.Log{Config: true, Var: "healthcheckRPC", ValStr: config.Healthcheck.Addr + ":" + config.Healthcheck.Port}

	if config.Node.Type == "forwarder" {
		healthChan <- nodehealth.Log{Config: true, Var: "primaryHealthcheckRPC", ValStr: config.Node.Forwarder.Target}
	}
	healthChan <- nodehealth.Log{Config: true, Var: "openethereumHealthcheckRPC", ValStr: config.L1.URL}
	nodehealth.Init(healthChan)

	var sequencerFeed chan broadcaster.BroadcastFeedMessage
	if len(config.Feed.Input.URLs) == 0 {
		logger.Warn().Msg("Missing --feed.url so not subscribing to feed")
	} else {
		sequencerFeed = make(chan broadcaster.BroadcastFeedMessage, 1)
		for _, url := range config.Feed.Input.URLs {
			broadcastClient := broadcastclient.NewBroadcastClient(url, nil, config.Feed.Input.Timeout)
			broadcastClient.ConnectInBackground(ctx, sequencerFeed)
		}
	}
	var inboxReader *monitor.InboxReader
	for {
		inboxReader, err = mon.StartInboxReader(ctx, l1Client, common.HexToAddress(config.Rollup.Address), config.Rollup.FromBlock, common.HexToAddress(config.BridgeUtilsAddress), healthChan, sequencerFeed)
		if err == nil {
			break
		}
		logger.Warn().Err(err).
			Str("url", config.L1.URL).
			Str("rollup", config.Rollup.Address).
			Str("bridgeUtils", config.BridgeUtilsAddress).
			Msg("failed to start inbox reader, waiting and retrying")

		select {
		case <-ctx.Done():
			return errors.New("ctx cancelled StartInboxReader retry loop")
		case <-time.After(5 * time.Second):
		}
	}

	var dataSigner func([]byte) ([]byte, error)
	var batcherMode rpc.BatcherMode
	if config.Node.Type == "forwarder" {
		logger.Info().Str("forwardTxURL", config.Node.Forwarder.Target).Msg("Arbitrum node starting in forwarder mode")
		batcherMode = rpc.ForwarderBatcherMode{NodeURL: config.Node.Forwarder.Target}
	} else {
		var auth *bind.TransactOpts
		auth, dataSigner, err = cmdhelp.GetKeystore(config.Persistent.Chain, wallet, config.GasPrice, l1ChainId)
		if err != nil {
			return errors.Wrap(err, "error running GetKeystore")
		}

		logger.Info().Hex("from", auth.From.Bytes()).Msg("Arbitrum node submitting batches")

		if err := ethbridge.WaitForBalance(
			ctx,
			l1Client,
			common.Address{},
			common.NewAddressFromEth(auth.From),
		); err != nil {
			return errors.Wrap(err, "error waiting for balance")
		}

		if config.Node.Type == "sequencer" {
			batcherMode = rpc.SequencerBatcherMode{
				Auth:                       auth,
				Core:                       mon.Core,
				InboxReader:                inboxReader,
				DelayedMessagesTargetDelay: big.NewInt(config.Node.Sequencer.DelayedMessagesTargetDelay),
				CreateBatchBlockInterval:   big.NewInt(config.Node.Sequencer.CreateBatchBlockInterval),
			}
		} else {
			inboxAddress := common.HexToAddress(config.Node.Aggregator.InboxAddress)
			if config.Node.Aggregator.Stateful {
				batcherMode = rpc.StatefulBatcherMode{Auth: auth, InboxAddress: inboxAddress}
			} else {
				batcherMode = rpc.StatelessBatcherMode{Auth: auth, InboxAddress: inboxAddress}
			}
		}
	}

	nodeStore := mon.Storage.GetNodeStore()
	metrics.RegisterNodeStoreMetrics(nodeStore, metricsConfig)
	db, txDBErrChan, err := txdb.New(ctx, mon.Core, nodeStore, 100*time.Millisecond)
	if err != nil {
		return errors.Wrap(err, "error opening txdb")
	}
	defer db.Close()

	if config.WaitToCatchUp {
		inboxReader.WaitToCatchUp(ctx)
	}

	var batch batcher.TransactionBatcher
	errChan := make(chan error, 1)
	for {
		batch, err = rpc.SetupBatcher(
			ctx,
			l1Client,
			rollupAddress,
			l2ChainId,
			db,
			time.Duration(config.Node.Aggregator.MaxBatchTime)*time.Second,
			batcherMode,
			dataSigner,
			config,
		)
		lockoutConf := config.Node.Sequencer.Lockout
		if err == nil && lockoutConf.Redis != "" {
			batch, err = rpc.SetupLockout(ctx, batch.(*batcher.SequencerBatcher), mon.Core, inboxReader, lockoutConf, errChan)
		}
		if err == nil {
			go batch.Start(ctx)
			break
		}
		logger.Warn().Err(err).Msg("failed to setup batcher, waiting and retrying")

		select {
		case <-ctx.Done():
			return errors.New("ctx cancelled setup batcher")
		case <-time.After(5 * time.Second):
		}
	}

	metricsConfig.RegisterSystemMetrics()
	metricsConfig.RegisterStaticMetrics()

	srv := aggregator.NewServer(batch, rollupAddress, l2ChainId, db)
	web3Server, err := web3.GenerateWeb3Server(srv, nil, false, nil, metricsConfig)
	if err != nil {
		return err
	}
	go func() {
		err := rpc.LaunchPublicServer(ctx, web3Server, config.Node.RPC.Addr, config.Node.RPC.Port, config.Node.WS.Addr, config.Node.WS.Port)
		if err != nil {
			errChan <- err
		}
	}()

	select {
	case err := <-txDBErrChan:
		return err
	case err := <-errChan:
		return err
	case <-cancelChan:
		return nil
	}
}
