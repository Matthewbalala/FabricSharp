/*
Copyright IBM Corp. All Rights Reserved.
SPDX-License-Identifier: Apache-2.0
*/

package stateustoredb

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"ustore"

	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/core/chaincode/shim"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/statedb"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/version"
	"github.com/pkg/errors"
)

var logger = flogging.MustGetLogger("stateustoredb")

var compositeKeySep = []byte{0x00}
var lastKeyIndicator = byte(0x01)
var savePointKey = []byte{0x00}

// VersionedDBProvider implements interface VersionedDBProvider
type VersionedDBProvider struct {
}

// NewVersionedDBProvider instantiates VersionedDBProvider
func NewVersionedDBProvider() *VersionedDBProvider {
	logger.Debug("constructing VersionedDBProvider for ustoredb")
	return &VersionedDBProvider{}
}

// GetDBHandle gets the handle to a named database
func (provider *VersionedDBProvider) GetDBHandle(dbName string) (statedb.VersionedDB, error) {
	return newVersionedDB(ustore.NewKVDB(), dbName), nil
}

// Close closes the underlying db
func (provider *VersionedDBProvider) Close() {
}

// VersionedDB implements VersionedDB interface
type versionedDB struct {
	lastSnapshot     uint64
	snapshotVersions map[uint64]string
	udb              ustore.KVDB
	dbName           string
}

// newVersionedDB constructs an instance of VersionedDB
func newVersionedDB(udb ustore.KVDB, dbName string) *versionedDB {
	return &versionedDB{0, make(map[uint64]string), udb, dbName}
}

// Open implements method in VersionedDB interface
func (vdb *versionedDB) Open() error {
	if status := vdb.udb.InitGlobalState(); !status.Ok() {
		return errors.New("Fail to init state with status" + status.ToString())
	}
	return nil
}

func (vdb *versionedDB) BytesKeySupported() bool {
	return false
}

// Close implements method in VersionedDB interface
func (vdb *versionedDB) Close() {
	// do nothing because shared db is used
}

// ValidateKeyValue implements method in VersionedDB interface
func (vdb *versionedDB) ValidateKeyValue(key string, value []byte) error {
	return nil
}

// BytesKeySuppoted implements method in VersionedDB interface
func (vdb *versionedDB) BytesKeySuppoted() bool {
	return true
}

// GetState implements method in VersionedDB interface
func (vdb *versionedDB) GetState(namespace string, key string) (*statedb.VersionedValue, error) {
	return vdb.GetSnapshotState(math.MaxUint64, namespace, key)
}

// GetVersion implements method in VersionedDB interface
func (vdb *versionedDB) GetVersion(namespace string, key string) (*version.Height, error) {
	if strings.HasSuffix(key, "_hist") {
		return version.NewHeight(0, 0), nil
	} else if strings.HasSuffix(key, "_backward") {
		return version.NewHeight(0, 0), nil
	} else if strings.HasSuffix(key, "_forward") {
		return version.NewHeight(0, 0), nil
	} else {
		versionedValue, err := vdb.GetState(namespace, key)
		if err != nil {
			return nil, err
		}
		if versionedValue == nil {
			return nil, nil
		}
		return versionedValue.Version, nil
	}
}

func (vdb *versionedDB) RetrieveLatestSnapshot() uint64 {
	return atomic.LoadUint64(&vdb.lastSnapshot)
}

func (vdb *versionedDB) ReleaseSnapshot(snapshot uint64) bool {
	// by right, we should remove the staled entry in snapshotHashes
	// But we do not bother to do so as they occupy so few space.
	return true
}

func (vdb *versionedDB) GetSnapshotState(snapshot uint64, namespace string, key string) (*statedb.VersionedValue, error) {
	logger.Infof("Get ns %s, key %s at snapshot %d", namespace, key, snapshot)
	zeroVer := version.NewHeight(0, 0)
	if strings.HasSuffix(key, "_hist") {
		splits := strings.Split(key, "_")
		originalKey := splits[0]
		queriedBlkIdx, err := strconv.Atoi(splits[1])
		if err != nil {
			return nil, errors.New("Fail to parse block index from Hist Query " + key)
		}

		var histResult shim.HistResult
		compositeKey := constructCompositeKey(namespace, originalKey)
		if histReturn := vdb.udb.Hist(compositeKey, uint64(queriedBlkIdx)); !histReturn.Status().Ok() {
			logger.Infof("Fail to query historical state for Key %s, at blk_idx %d with status %s",
				compositeKey, queriedBlkIdx, histReturn.Status().ToString())
			histResult = shim.HistResult{Msg: histReturn.Status().ToString(), Val: "", CreatedBlk: 0}
		} else {
			histVal := histReturn.Value()
			height := histReturn.Blk_idx()
			logger.Infof("ustoredb.Hist(%s, %d) = (%s, %d)", compositeKey, queriedBlkIdx, histVal, height)
			histResult = shim.HistResult{Msg: "", Val: histVal, CreatedBlk: height}
		}
		if histJSON, err := json.Marshal(histResult); err != nil {
			return nil, errors.New("Fail to marshal for HistResult")
		} else {
			return &statedb.VersionedValue{Version: zeroVer, Value: histJSON, Metadata: nil}, nil
		}
	} else if strings.HasSuffix(key, "_backward") {
		splits := strings.Split(key, "_")
		originalKey := splits[0]
		queriedBlkIdx, err := strconv.Atoi(splits[1])
		if err != nil {
			return nil, errors.New("Fail to parse block index from Backward Query " + key)
		}

		var backResult shim.BackwardResult
		compositeKey := constructCompositeKey(namespace, originalKey)
		if backReturn := vdb.udb.Backward(compositeKey, uint64(queriedBlkIdx)); !backReturn.Status().Ok() {
			logger.Infof("Fail to backward query for Key %s at blk_idx %d with status %d", compositeKey, queriedBlkIdx, backReturn.Status().ToString())

			backResult = shim.BackwardResult{Msg: backReturn.Status().ToString(), DepKeys: nil, DepBlkIdx: nil, TxnID: ""}
		} else {
			depKeys := make([]string, 0)
			depBlkIdxs := make([]uint64, 0)

			for i := 0; i < int(backReturn.Dep_keys().Size()); i++ {
				depKeys = append(depKeys, backReturn.Dep_keys().Get(i))
				depBlkIdxs = append(depBlkIdxs, backReturn.Dep_blk_idx().Get(i))
			}

			logger.Infof("ustoredb.Backward(%s, %d) = (%v, %v)", compositeKey, queriedBlkIdx, depKeys, depBlkIdxs)
			backResult = shim.BackwardResult{Msg: "", DepKeys: depKeys, DepBlkIdx: depBlkIdxs, TxnID: backReturn.TxnID()}
		}
		if backJSON, err := json.Marshal(backResult); err != nil {
			return nil, errors.New("Fail to marshal for backResult")
		} else {
			return &statedb.VersionedValue{Version: zeroVer, Value: backJSON, Metadata: nil}, nil
		}
	} else if strings.HasSuffix(key, "_forward") {
		splits := strings.Split(key, "_")
		originalKey := splits[0]
		queriedBlkIdx, err := strconv.Atoi(splits[1])
		if err != nil {
			return nil, errors.New("Fail to parse block index from Forward Query " + key)
		}

		var forwardResult shim.ForwardResult
		compositeKey := constructCompositeKey(namespace, originalKey)
		if forwardReturn := vdb.udb.Forward(compositeKey, uint64(queriedBlkIdx)); !forwardReturn.Status().Ok() {
			logger.Infof("Fail to forward query for Key %s at blk_idx %d with status %d", compositeKey, queriedBlkIdx, forwardReturn.Status().ToString())

			forwardResult = shim.ForwardResult{Msg: forwardReturn.Status().ToString(), ForwardKeys: nil, ForwardBlkIdx: nil, ForwardTxnIDs: nil}
		} else {
			forKeys := make([]string, 0)
			forBlkIdxs := make([]uint64, 0)
			forTxnIDs := make([]string, 0)

			for i := 0; i < int(forwardReturn.Forward_keys().Size()); i++ {
				forKeys = append(forKeys, forwardReturn.Forward_keys().Get(i))
				forBlkIdxs = append(forBlkIdxs, forwardReturn.Forward_blk_idx().Get(i))
				forTxnIDs = append(forTxnIDs, forwardReturn.TxnIDs().Get(i))
			}

			logger.Infof("ustoredb.Backward(%s, %d) = (%v, %v, %v)", compositeKey, queriedBlkIdx, forKeys, forBlkIdxs, forTxnIDs)
			forwardResult = shim.ForwardResult{Msg: "", ForwardKeys: forKeys, ForwardBlkIdx: forBlkIdxs, ForwardTxnIDs: forTxnIDs}
		}
		if forwardJSON, err := json.Marshal(forwardResult); err != nil {
			return nil, errors.New("Fail to marshal for forwardResult")
		} else {
			return &statedb.VersionedValue{Version: zeroVer, Value: forwardJSON, Metadata: nil}, nil
		}
	} else {
		compositeKey := constructCompositeKey(namespace, key)
		if histResult := vdb.udb.Hist(compositeKey, snapshot); histResult.Status().IsNotFound() {
			return nil, nil
		} else if histResult.Status().Ok() {
			val := []byte(histResult.Value())
			height := histResult.Blk_idx()
			logger.Infof("ustoredb.SnapshotGetState(). ns=%s, snapshot=%d, key=%s, val=%s, blk_idx=%d", namespace, snapshot, key, val, height)
			ver := version.NewHeight(snapshot, 0)
			if snapshot == math.MaxUint64 { // it is called by the GetState() or GetVersion()
				ver = version.NewHeight(height, 0)
			}
			return &statedb.VersionedValue{Version: ver, Value: val, Metadata: nil}, nil
		} else {
			return nil, errors.New("Fail to get state for Key " + compositeKey + " with status " + histResult.Status().ToString())
		}
	}
}

// ApplyUpdates implements method in VersionedDB interface
func (vdb *versionedDB) ApplyUpdates(batch *statedb.UpdateBatch, height *version.Height) error {
	// dbBatch := leveldbhelper.NewUpdateBatch()
	namespaces := batch.GetUpdatedNamespaces()
	logger.Infof("[udb] Prepare to commit blk %d", uint64(height.BlockNum))
	for i, ns := range namespaces {
		logger.Infof("[udb] Prepare to commit %d ns %s", i, ns)
		updates := batch.GetUpdates(ns)
		for k, vv := range updates {
			compositeKey := constructCompositeKey(ns, k)
			logger.Infof("[udb] ApplyUpdates: Channel [%s]: Applying key(string)=[%s] value(string)=[%s]", vdb.dbName, string(compositeKey), string(vv.Value))
			if !strings.HasSuffix(k, "_prov") && !strings.HasSuffix(k, "_txnID") && !strings.HasSuffix(k, "_snapshot") {
				// logger.Infof("[udb] Key %s is normal", k)
				val := string(vv.Value)
				depList := ustore.NewVecStr()
				depStrs := make([]string, 0)
				if provVal, ok := updates[k+"_prov"]; ok {
					depKeys := strings.Split(string(provVal.Value), "_")
					for _, depKey := range depKeys {
						if len(depKey) > 0 {
							depCompKey := constructCompositeKey(ns, depKey)
							depList.Add(depCompKey)
							depStrs = append(depStrs, depCompKey)
						}
					} // end for
				} // end if provVal
				// logger.Infof("Temp Disable for dependency...")
				txnID := "faketxnid" // can NOT be empty
				if txnIDVal, ok := updates[k+"_txnID"]; ok {
					txnID = string(txnIDVal.Value)
				}
				var snapshotVersion string
				var snapshot uint64
				if snapshotVal, ok := updates[k+"_snapshot"]; ok {
					snapshot = binary.LittleEndian.Uint64(snapshotVal.Value)
					if snapshot == math.MaxUint64 {
						// this could happen if the txn is update-only.
						snapshotVersion = ""
					} else {
						snapshotVersion = vdb.snapshotVersions[snapshot]
					}
				} else {
					snapshotVersion = ""
					// 	panic(fmt.Sprintf("Fail to find the snapshot for key %s", k))
				}

				startPut := time.Now()
				vdb.udb.PutState(compositeKey, val, txnID, height.BlockNum, depList, snapshotVersion)
				elapsedPut := time.Since(startPut).Nanoseconds() / 1000
				logger.Infof("[udb] PutState key [%s], val [%s], txnID [%s], blk idx [%d], dep_list [%v], snapshot=%d with %d us", compositeKey, val, txnID, height.BlockNum, depStrs, snapshot, elapsedPut)
			} else {
				logger.Infof("[udb] Key %s has special prov or txnID suffix", k)
			} // end if has Suffix
		}
	}
	blkIdx := height.BlockNum
	startCommit := time.Now()
	logger.Infof("[udb] Finish apply batch updates for block %d", blkIdx)
	if statusStr := vdb.udb.Commit(); !statusStr.GetFirst().Ok() {
		return errors.New("Fail to commit global state with status " + statusStr.GetFirst().ToString())
	} else {
		newVersion := statusStr.GetSecond()
		vdb.snapshotVersions[blkIdx] = newVersion
		atomic.StoreUint64(&vdb.lastSnapshot, blkIdx)
	}
	elapsedCommit := time.Since(startCommit).Nanoseconds() / 1000
	logger.Infof("[udb] Finish commit state for block %d with %d us", blkIdx, elapsedCommit)
	if status := vdb.udb.Put("latest-height", strconv.Itoa(int(blkIdx))); !status.Ok() {
		return errors.New("Fail to put latest block height with status " + status.ToString())
	}
	return nil
}

// GetLatestSavePoint implements method in VersionedDB interface
func (vdb *versionedDB) GetLatestSavePoint() (*version.Height, error) {
	if statusStr := vdb.udb.Get("latest-height"); !statusStr.GetFirst().Ok() {
		return nil, errors.New("Fail to get latest height with msg " + statusStr.GetFirst().ToString())
	} else if blkIdx, err := strconv.Atoi(statusStr.GetSecond()); err != nil {
		return nil, errors.New("Fail to parse latest blk idx from string")
	} else {
		return version.NewHeight(uint64(blkIdx), 0), nil
	}
}

// GetStateMultipleKeys implements method in VersionedDB interface
func (vdb *versionedDB) GetStateMultipleKeys(namespace string, keys []string) ([]*statedb.VersionedValue, error) {
	return nil, errors.New("GetStateMultipleKeys not supported for ustoredb")
}

// GetStateRangeScanIterator implements method in VersionedDB interface
// startKey is inclusive
// endKey is exclusive
func (vdb *versionedDB) GetStateRangeScanIterator(namespace string, startKey string, endKey string) (statedb.ResultsIterator, error) {
	return nil, errors.New("GetStateRangeScanIterator not supported for ustoredb")
	// return vdb.GetStateRangeScanIteratorWithMetadata(namespace, startKey, endKey, nil)
}

// GetStateRangeScanIteratorWithMetadata implements method in VersionedDB interface
func (vdb *versionedDB) GetStateRangeScanIteratorWithMetadata(namespace string, startKey string, endKey string, metadata map[string]interface{}) (statedb.QueryResultsIterator, error) {
	return nil, errors.New("GetStateRangeScanIteratorWithMetadata not supported for ustoredb")
}

// ExecuteQuery implements method in VersionedDB interface
func (vdb *versionedDB) ExecuteQuery(namespace, query string) (statedb.ResultsIterator, error) {
	return nil, errors.New("ExecuteQuery not supported for ustoredb")
}

// ExecuteQueryWithMetadata implements method in VersionedDB interface
func (vdb *versionedDB) ExecuteQueryWithMetadata(namespace, query string, metadata map[string]interface{}) (statedb.QueryResultsIterator, error) {
	return nil, errors.New("ExecuteQueryWithMetadata not supported for ustoredb")
}

func constructCompositeKey(ns string, key string) string {
	return ns + "#" + key
}
