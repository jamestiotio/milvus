// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package flowgraph

import (
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/timerecord"
)

const (
	// TODO: better to be configured
	nodeCtxTtInterval = 2 * time.Minute
	enableTtChecker   = true
	// blockAll should wait no more than 10 seconds
	blockAllWait = 10 * time.Second
)

// Node is the interface defines the behavior of flowgraph
type Node interface {
	Name() string
	MaxQueueLength() int32
	MaxParallelism() int32
	IsValidInMsg(in []Msg) bool
	Operate(in []Msg) []Msg
	IsInputNode() bool
	Start()
	Close()
}

// BaseNode defines some common node attributes and behavior
type BaseNode struct {
	maxQueueLength int32
	maxParallelism int32
}

// manage nodeCtx
type nodeCtxManager struct {
	inputNodeCtx *nodeCtx
	closeWg      *sync.WaitGroup
	closeOnce    sync.Once

	inputNodeCloseCh chan struct{} // notify input node work to exit
	workNodeCh       chan struct{} // notify ddnode and downstream node work to exit
}

// NewNodeCtxManager init with the inputNode and fg.closeWg
func NewNodeCtxManager(nodeCtx *nodeCtx, closeWg *sync.WaitGroup) *nodeCtxManager {
	return &nodeCtxManager{
		inputNodeCtx:     nodeCtx,
		closeWg:          closeWg,
		inputNodeCloseCh: make(chan struct{}),
		workNodeCh:       make(chan struct{}),
	}
}

// Start invoke Node `Start` method and start a worker goroutine
func (nodeCtxManager *nodeCtxManager) Start() {
	// in dmInputNode, message from mq to channel, alloc goroutines
	// limit the goroutines in other node to prevent huge goroutines numbers
	nodeCtxManager.closeWg.Add(2)
	go nodeCtxManager.inputNodeStart()
	go nodeCtxManager.workNodeStart()
}

func (nodeCtxManager *nodeCtxManager) inputNodeStart() {
	defer nodeCtxManager.closeWg.Done()
	inputNode := nodeCtxManager.inputNodeCtx
	name := fmt.Sprintf("nodeCtxTtChecker-%s", inputNode.node.Name())
	// tt checker start
	var checker *timerecord.GroupChecker
	if enableTtChecker {
		checker = timerecord.GetGroupChecker("fgNode", nodeCtxTtInterval, func(list []string) {
			log.Warn("some node(s) haven't received input", zap.Strings("list", list), zap.Duration("duration ", nodeCtxTtInterval))
		})
		checker.Check(name)
		defer checker.Remove(name)
	}

	for {
		select {
		case <-nodeCtxManager.inputNodeCloseCh:
			return
		// handles node work spinning
		// 1. collectMessage from upstream or just produce Msg from InputNode
		// 2. invoke node.Operate
		// 3. deliver the Operate result to downstream nodes
		default:
			// inputs from inputsMessages for Operate
			var input, output []Msg
			// inputNode.input not from nodeCtx.inputChannel
			// the input message decides whether the operate method is executed
			n := inputNode.node
			inputNode.blockMutex.RLock()
			if !n.IsValidInMsg(input) {
				inputNode.blockMutex.RUnlock()
				continue
			}
			output = n.Operate(input)
			inputNode.blockMutex.RUnlock()
			// the output decide whether the node should be closed.
			if isCloseMsg(output) {
				close(nodeCtxManager.inputNodeCloseCh)
				// inputNode.Close()
				if inputNode.inputChannel != nil {
					close(inputNode.inputChannel)
				}
			}
			// deliver to all following flow graph node.
			inputNode.downstream.inputChannel <- output
			if enableTtChecker {
				checker.Check(name)
			}
		}
	}
}

func (nodeCtxManager *nodeCtxManager) workNodeStart() {
	defer nodeCtxManager.closeWg.Done()
	ddNode := nodeCtxManager.inputNodeCtx.downstream
	curNode := ddNode
	// tt checker start
	var checker *timerecord.GroupChecker
	if enableTtChecker {
		checker = timerecord.GetGroupChecker("fgNode", nodeCtxTtInterval, func(list []string) {
			log.Warn("some node(s) haven't received input", zap.Strings("list", list), zap.Duration("duration ", nodeCtxTtInterval))
		})
		for curNode != nil {
			name := fmt.Sprintf("nodeCtxTtChecker-%s", curNode.node.Name())
			checker.Check(name)
			curNode = curNode.downstream
			defer checker.Remove(name)
		}
	}

	for {
		select {
		case <-nodeCtxManager.workNodeCh:
			return
		// handles node work spinning
		// 1. collectMessage from upstream or just produce Msg from InputNode
		// 2. invoke node.Operate
		// 3. deliver the Operate result to downstream nodes
		default:
			// goroutine will work loop for all node(expect inpuNode) even when closeCh notify to exit
			// input node will close all node
			curNode = ddNode
			for curNode != nil {
				// inputs from inputsMessages for Operate
				var input, output []Msg
				input = <-curNode.inputChannel
				// the input message decides whether the operate method is executed
				n := curNode.node
				curNode.blockMutex.RLock()
				if !n.IsValidInMsg(input) {
					curNode.blockMutex.RUnlock()
					curNode = ddNode
					continue
				}

				output = n.Operate(input)
				curNode.blockMutex.RUnlock()
				// the output decide whether the node should be closed.
				if isCloseMsg(output) {
					nodeCtxManager.closeOnce.Do(func() {
						close(nodeCtxManager.workNodeCh)
					})
					if curNode.inputChannel != nil {
						close(curNode.inputChannel)
					}
				}
				// deliver to all following flow graph node.
				if curNode.downstream != nil {
					curNode.downstream.inputChannel <- output
				}
				if enableTtChecker {
					checker.Check(fmt.Sprintf("nodeCtxTtChecker-%s", curNode.node.Name()))
				}
				curNode = curNode.downstream
			}
		}
	}
}

// Close handles cleanup logic and notify worker to quit
func (nodeCtxManager *nodeCtxManager) Close() {
	nodeCtx := nodeCtxManager.inputNodeCtx
	nodeCtx.Close()
}

// nodeCtx maintains the running context for a Node in flowgragh
type nodeCtx struct {
	node         Node
	inputChannel chan []Msg
	downstream   *nodeCtx

	blockMutex sync.RWMutex
}

func (nodeCtx *nodeCtx) Block() {
	// input node operate function will be blocking
	if !nodeCtx.node.IsInputNode() {
		startTs := time.Now()
		nodeCtx.blockMutex.Lock()
		if time.Since(startTs) >= blockAllWait {
			log.Warn("flow graph wait for long time",
				zap.String("name", nodeCtx.node.Name()),
				zap.Duration("wait time", time.Since(startTs)))
		}
	}
}

func (nodeCtx *nodeCtx) Unblock() {
	if !nodeCtx.node.IsInputNode() {
		nodeCtx.blockMutex.Unlock()
	}
}

func isCloseMsg(msgs []Msg) bool {
	if len(msgs) == 1 {
		return msgs[0].IsClose()
	}
	return false
}

// Close handles cleanup logic and notify worker to quit
func (nodeCtx *nodeCtx) Close() {
	if nodeCtx.node.IsInputNode() {
		for nodeCtx != nil {
			nodeCtx.node.Close()
			log.Debug("flow graph node closed", zap.String("nodeName", nodeCtx.node.Name()))
			nodeCtx = nodeCtx.downstream
		}
	}
}

// MaxQueueLength returns the maximal queue length
func (node *BaseNode) MaxQueueLength() int32 {
	return node.maxQueueLength
}

// MaxParallelism returns the maximal parallelism
func (node *BaseNode) MaxParallelism() int32 {
	return node.maxParallelism
}

// SetMaxQueueLength is used to set the maximal queue length
func (node *BaseNode) SetMaxQueueLength(n int32) {
	node.maxQueueLength = n
}

// SetMaxParallelism is used to set the maximal parallelism
func (node *BaseNode) SetMaxParallelism(n int32) {
	node.maxParallelism = n
}

// IsInputNode returns whether Node is InputNode, BaseNode is not InputNode by default
func (node *BaseNode) IsInputNode() bool {
	return false
}

// Start implementing Node, base node does nothing when starts
func (node *BaseNode) Start() {}

// Close implementing Node, base node does nothing when stops
func (node *BaseNode) Close() {}

func (node *BaseNode) Name() string {
	return "BaseNode"
}

func (node *BaseNode) Operate(in []Msg) []Msg {
	return in
}

func (node *BaseNode) IsValidInMsg(in []Msg) bool {
	if in == nil {
		log.Info("type assertion failed because it's nil")
		return false
	}

	if len(in) == 0 {
		// avoid printing too many logs.
		return false
	}

	if len(in) != 1 {
		log.Warn("Invalid operate message input", zap.Int("input length", len(in)))
		return false
	}
	return true
}
