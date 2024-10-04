package evm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ava-labs/coreth/core"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/eth/tracers"
	"github.com/ava-labs/coreth/utils"
	"github.com/ethereum/go-ethereum/log"
)

func (vm *VM) script() error {
	vm.blockChain.Stop()

	vmConfig := vm.blockChain.GetVMConfig()
	cacheConfig := *vm.blockChain.GetCacheConfig()
	// Disable snapshotting
	cacheConfig.SnapshotDelayInit = true
	cacheConfig.SnapshotLimit = 0

	progress := make(chan *types.Block, 1)
	errChan := make(chan error, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var (
			processed     uint64
			lastProcessed uint64
			start         = time.Now()
			last          = start
			update        = 5 * time.Second
		)
		for range progress {
			processed++
			if time.Since(last) > update {
				last = time.Now()
				log.Info(
					"reprocessing",
					"processed", processed,
					"new", processed-lastProcessed,
					"blocks/s", float64(processed-lastProcessed)/update.Seconds(),
				)
				lastProcessed = processed
			}
		}
		log.Info(
			"reprocessing finished",
			"processed", processed,
			"duration", time.Since(start),
			"blocks/s", float64(processed)/time.Since(start).Seconds(),
		)
	}()

	work := func(from, to uint64) func() {
		return func() {
			doneFile := fmt.Sprintf("reprocess-%d.done", to)
			if _, err := os.Stat(doneFile); err == nil {
				log.Info("skipping reprocess", "from", from, "to", to)
				return
			}

			chain, err := core.NewBlockChain(
				vm.chaindb,
				&cacheConfig,
				vm.ethConfig.Genesis,
				vm.blockChain.Engine(),
				*vmConfig,
				vm.blockChain.LastAcceptedBlock().Hash(),
				false)
			if err != nil {
				log.Error("[REPROCESS FAILED] failed to create new blockchain", "err", err)
				errChan <- fmt.Errorf("failed to create new blockchain: %w", err)
				return
			}
			if err := chain.Reprocess(from, to, progress); err != nil {
				log.Error("[REPROCESS FAILED] to reprocess blockchain", "err", err)
				errChan <- fmt.Errorf("failed to reprocess blockchain: %w", err)
				return
			}
			chain.Stop()

			if err := os.WriteFile(doneFile, []byte{}, 0644); err != nil {
				log.Error("[REPROCESS FAILED] failed to write done file", "err", err)
				errChan <- fmt.Errorf("failed to write done file: %w", err)
				return
			}
		}
	}

	numWorkers := 8
	if env := os.Getenv("BLOCK_REPROCESS_WORKERS"); env != "" {
		parsed, err := strconv.Atoi(env)
		if err != nil {
			return err
		}
		numWorkers = parsed
	}

	var err error
	stride := uint64(4096 * 100)
	workers := utils.NewBoundedWorkers(numWorkers)
	startAt := uint64(0)
	upTo := vm.blockChain.LastAcceptedBlock().NumberU64()

	if env := os.Getenv("BLOCK_REPROCESS_START"); env != "" {
		parsed, err := strconv.Atoi(env)
		if err != nil {
			return err
		}
		if uint64(parsed)%stride != 0 {
			return fmt.Errorf("start block must be a multiple of %d", stride)
		}
		startAt = uint64(parsed) / stride
	}
	if env := os.Getenv("BLOCK_REPROCESS_END"); env != "" {
		parsed, err := strconv.Atoi(env)
		if err != nil {
			return err
		}
		if upTo > uint64(parsed) {
			upTo = uint64(parsed)
		}
	}
	log.Warn("REPROCESSING BLOCKCHAIN -- NOT FOR PRODUCTION", "start", startAt, "end", upTo, "numWorkers", numWorkers)

	for i := startAt; i*stride+1 < upTo; i++ {
		select {
		case err = <-errChan:
			break
		default:
		}

		from, to := i*stride+1, (i+1)*stride
		if to > upTo {
			to = upTo
		}
		workers.Execute(work(from, to))
	}
	workers.Wait()
	// Check error at the end to ensure all workers have finished
	select {
	case err = <-errChan:
	default:
	}

	close(progress)
	wg.Wait()
	if err != nil {
		log.Error("failed to reprocess blockchain", "err", err)
		return err
	}

	return errors.New("intentionally stopping VM from initializing")
}

func (vm *VM) traceBlock() error {
	number := uint64(0)
	if env := os.Getenv("BLOCK_REPROCESS_TRACE"); env != "" {
		parsed, err := strconv.Atoi(env)
		if err != nil {
			return err
		}
		number = uint64(parsed)
	}
	if number == 0 {
		return nil // contiune
	}

	ft := tracers.NewFileTracerAPI(vm.eth.APIBackend)
	block := vm.blockChain.GetBlockByNumber(number)
	outs, err := ft.StandardTraceBlockToFile(
		context.Background(),
		block.Hash(),
		&tracers.StdTraceConfig{
			Reexec: utils.NewUint64(4096),
		},
	)
	log.Info("tracing block", "block", number, "err", err)
	for _, out := range outs {
		log.Info("tracing output", "block", number, "out", out)
	}
	return errors.New("intentionally stopping VM from initializing (trace)")
}
