// Copyright 2022 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package ec

import (
	"bytes"
	"crypto/rand"
	mrand "math/rand"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/cubefs/cubefs/blobstore/common/codemode"
	"github.com/cubefs/cubefs/blobstore/util/bytespool"
)

var srcData = []byte("Hello world")

func copyShards(a [][]byte) [][]byte {
	b := make([][]byte, len(a))
	for i := range a {
		b[i] = append(b[i], a[i]...)
	}
	return b
}

func TestEncoderNew(t *testing.T) {
	{
		_, err := NewEncoder(Config{CodeMode: codemode.Tactic{}})
		assert.ErrorIs(t, err, ErrInvalidCodeMode)
	}
	{
		_, err := NewEncoder(Config{CodeMode: codemode.EC15P12.Tactic()})
		assert.NoError(t, err)
		_, err = NewEncoder(Config{CodeMode: codemode.EC16P20L2.Tactic()})
		assert.NoError(t, err)
	}
}

func TestEncoder(t *testing.T) {
	cfg := Config{
		CodeMode:     codemode.EC15P12.Tactic(),
		EnableVerify: true,
		Concurrency:  10,
	}
	encoder, err := NewEncoder(cfg)
	assert.NoError(t, err)

	// source data split
	shards, err := encoder.Split(srcData)
	assert.NoError(t, err)

	// encode data
	err = encoder.Encode(shards)
	assert.NoError(t, err)
	wbuff := bytes.NewBuffer(make([]byte, 0))
	err = encoder.Join(wbuff, shards, len(srcData))
	assert.NoError(t, err)
	assert.Equal(t, srcData, wbuff.Bytes())

	dataShards := encoder.GetDataShards(shards)
	// set one data shards broken
	for i := range dataShards[0] {
		dataShards[0][i] = 222
	}
	// reconstruct data and check
	err = encoder.ReconstructData(shards, []int{0})
	assert.NoError(t, err)
	wbuff = bytes.NewBuffer(make([]byte, 0))
	err = encoder.Join(wbuff, shards, len(srcData))
	assert.NoError(t, err)
	assert.Equal(t, srcData, wbuff.Bytes())

	// reconstruct shard and check
	parityShards := encoder.GetParityShards(shards)
	for i := range parityShards[1] {
		parityShards[1][i] = 11
	}
	err = encoder.Reconstruct(shards, []int{cfg.CodeMode.N + 1})
	assert.NoError(t, err)
	ok, err := encoder.Verify(shards)
	assert.NoError(t, err)
	assert.True(t, ok)
	wbuff = bytes.NewBuffer(make([]byte, 0))
	err = encoder.Join(wbuff, shards, len(srcData))
	assert.NoError(t, err)
	assert.Equal(t, srcData, wbuff.Bytes())

	ls := encoder.GetLocalShards(shards)
	assert.Equal(t, 0, len(ls))
	si := encoder.GetShardsInIdc(shards, 0)
	assert.Equal(t, (cfg.CodeMode.N+cfg.CodeMode.M)/3, len(si))
}

func TestLrcEncoder(t *testing.T) {
	cfg := Config{
		CodeMode:     codemode.EC6P10L2.Tactic(),
		EnableVerify: true,
	}
	encoder, err := NewEncoder(cfg)
	assert.NoError(t, err)

	// source data split
	shards, err := encoder.Split(srcData)
	assert.NoError(t, err)
	{
		enoughBuff := make([]byte, 1<<10)
		copy(enoughBuff, srcData)
		enoughBuff = enoughBuff[:len(srcData)]
		_, err := encoder.Split(enoughBuff)
		assert.NoError(t, err)
	}

	invalidShards := shards[:len(shards)-1]
	assert.ErrorIs(t, encoder.Encode(invalidShards), ErrInvalidShards)
	assert.ErrorIs(t, encoder.Encode(nil), ErrInvalidShards)

	// encode data
	err = encoder.Encode(shards)
	assert.NoError(t, err)
	wbuff := bytes.NewBuffer(make([]byte, 0))
	err = encoder.Join(wbuff, shards, len(srcData))
	assert.NoError(t, err)
	assert.Equal(t, srcData, wbuff.Bytes())

	dataShards := encoder.GetDataShards(shards)
	// set one data shard broken
	for i := range dataShards[0] {
		dataShards[0][i] = 222
	}
	// reconstruct data and check
	err = encoder.ReconstructData(shards, []int{0})
	assert.NoError(t, err)
	wbuff = bytes.NewBuffer(make([]byte, 0))
	err = encoder.Join(wbuff, shards, len(srcData))
	assert.NoError(t, err)
	assert.Equal(t, srcData, wbuff.Bytes())

	// Local reconstruct shard and check
	localShardsInIdc := encoder.GetShardsInIdc(shards, 0)
	for idx := 0; idx < len(localShardsInIdc); idx++ {
		// set wrong data
		for i := range localShardsInIdc[idx] {
			localShardsInIdc[idx][i] = 11
		}
		// check must be false when a shard broken
		ok, err := encoder.Verify(shards)
		assert.NoError(t, err)
		assert.False(t, ok)

		err = encoder.Reconstruct(localShardsInIdc, []int{idx})
		assert.NoError(t, err)
		ok, err = encoder.Verify(shards)
		assert.NoError(t, err)
		assert.True(t, ok)
	}

	// global reconstruct shard and check
	dataShards = encoder.GetDataShards(shards)
	parityShards := encoder.GetParityShards(shards)
	badIdxs := make([]int, 0)
	for i := 0; i < cfg.CodeMode.M; i++ {
		if i%2 == 0 {
			badIdxs = append(badIdxs, i)
			// set wrong data
			if i < len(dataShards) {
				for j := range dataShards[i] {
					dataShards[i][j] = 222
				}
			}
		} else {
			badIdxs = append(badIdxs, cfg.CodeMode.N+i)
			// set wrong data
			for j := range parityShards[i] {
				parityShards[i][j] = 222
			}
		}
	}
	// add a local broken
	for j := range shards[cfg.CodeMode.N+cfg.CodeMode.M+1] {
		shards[cfg.CodeMode.N+cfg.CodeMode.M+1][j] = 222
	}
	badIdxs = append(badIdxs, cfg.CodeMode.N+cfg.CodeMode.M+1)
	err = encoder.Reconstruct(shards, badIdxs)
	assert.NoError(t, err)
	ok, err := encoder.Verify(shards)
	assert.NoError(t, err)
	assert.True(t, ok)
	wbuff = bytes.NewBuffer(make([]byte, 0))
	err = encoder.Join(wbuff, shards, len(srcData))
	assert.NoError(t, err)
	assert.Equal(t, srcData, wbuff.Bytes())

	ls := encoder.GetLocalShards(shards)
	assert.Equal(t, cfg.CodeMode.L, len(ls))
	si := encoder.GetShardsInIdc(shards, 0)
	assert.Equal(t, (cfg.CodeMode.N+cfg.CodeMode.M+cfg.CodeMode.L)/cfg.CodeMode.AZCount, len(si))
}

func TestLrcReconstruct(t *testing.T) {
	for _, cm := range codemode.GetAllCodeModes() {
		testLrcReconstruct(t, cm)
	}
}

func testLrcReconstruct(t *testing.T, cm codemode.CodeMode) {
	tactic := cm.Tactic()
	cfg := Config{CodeMode: tactic, EnableVerify: true}
	encoder, _ := NewEncoder(cfg)

	data := make([]byte, (1<<16)+mrand.Intn(1<<16))
	rand.Read(data)

	shards, err := encoder.Split(data)
	assert.NoError(t, err)
	assert.NoError(t, encoder.Encode(shards))

	origin := copyShards(shards)
	bads := make([]int, 0)
	for badIdx := tactic.N + tactic.M; badIdx < cm.GetShardNum(); badIdx++ {
		bads = append(bads, badIdx)
		for _, idx := range bads {
			bytespool.Zero(shards[idx])
			shards[idx] = shards[idx][:0]
		}
		assert.NoError(t, encoder.Reconstruct(shards, bads))
		assert.True(t, reflect.DeepEqual(origin, shards))
	}
	for badIdx := 0; badIdx < tactic.N+tactic.M; badIdx++ {
		bads = append(bads, badIdx)
	}
	assert.Error(t, encoder.Reconstruct(copyShards(shards), bads))

	// use local ec reconstruct
	for azIdx := 0; azIdx < tactic.AZCount; azIdx++ {
		locals, n, m := tactic.LocalStripeInAZ(azIdx)
		var localShards [][]byte
		for _, idx := range locals {
			localShards = append(localShards, shards[idx])
		}
		localOrigin := copyShards(localShards)

		bads := make([]int, 0)
		for badIdx := n; badIdx < n+m; badIdx++ {
			bads = append(bads, badIdx)
			for _, idx := range bads {
				bytespool.Zero(localShards[idx])
				localShards[idx] = localShards[idx][:0]
			}
			assert.NoError(t, encoder.Reconstruct(localShards, bads))
			assert.True(t, reflect.DeepEqual(localOrigin, localShards))
		}
		if n > 0 {
			bads = append(bads, n-1)
			assert.Error(t, encoder.Reconstruct(localShards, bads))
		}
	}
}
