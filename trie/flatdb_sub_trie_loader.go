package trie

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/ledgerwatch/bolt"
	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/common/debug"
	"github.com/ledgerwatch/turbo-geth/core/types/accounts"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/metrics"
	"github.com/ledgerwatch/turbo-geth/trie/rlphacks"
)

var (
	trieFlatDbSubTrieLoaderTimer = metrics.NewRegisteredTimer("trie/subtrieloader/flatdb", nil)
)

type FlatDbSubTrieLoader struct {
	rl       RetainDecider
	curr     bytes.Buffer // Current key for the structure generation algorithm, as well as the input tape for the hash builder
	succ     bytes.Buffer
	value    bytes.Buffer // Current value to be used as the value tape for the hash builder
	groups   []uint16
	hb       *HashBuilder
	rangeIdx int
	a        accounts.Account
	leafData GenStructStepLeafData
	accData  GenStructStepAccountData

	subTries SubTries

	wasIH        bool
	wasIHStorage bool
	hashData     GenStructStepHashData
	trace        bool

	currStorage  bytes.Buffer // Current key for the structure generation algorithm, as well as the input tape for the hash builder
	succStorage  bytes.Buffer
	valueStorage bytes.Buffer // Current value to be used as the value tape for the hash builder

	witnessLenAccount uint64
	witnessLenStorage uint64

	accAddrHashWithInc [40]byte // Concatenation of addrHash of the currently build account with its incarnation encoding
	dbPrefixes         [][]byte
	fixedbytes         []int
	masks              []byte
	cutoffs            []int
	boltDB             *bolt.DB
	nextAccountKey     [32]byte
	k, v               []byte
	ihK, ihV           []byte
	minKeyAsNibbles    bytes.Buffer

	itemPresent   bool
	itemType      StreamItem
	getWitnessLen func(prefix []byte) uint64

	// Storage item buffer
	storageKeyPart1   []byte
	storageKeyPart2   []byte
	storageHash       []byte
	storageValue      []byte
	storageWitnessLen uint64

	// Acount item buffer
	accountKey        []byte
	accountHash       []byte
	accountValue      accounts.Account
	streamCutoff      int
	accountWitnessLen uint64
}

func NewFlatDbSubTrieLoader() *FlatDbSubTrieLoader {
	fstl := &FlatDbSubTrieLoader{
		hb: NewHashBuilder(false),
	}
	return fstl
}

// Reset prepares the loader for reuse
func (fstl *FlatDbSubTrieLoader) Reset(db ethdb.Database, rl RetainDecider, dbPrefixes [][]byte, fixedbits []int, trace bool) error {
	fstl.rangeIdx = 0
	fstl.curr.Reset()
	fstl.succ.Reset()
	fstl.value.Reset()
	fstl.groups = fstl.groups[:0]
	fstl.a.Reset()
	fstl.hb.Reset()
	fstl.wasIH = false

	fstl.currStorage.Reset()
	fstl.succStorage.Reset()
	fstl.valueStorage.Reset()
	fstl.minKeyAsNibbles.Reset()
	fstl.wasIHStorage = false
	fstl.subTries = SubTries{}
	fstl.trace = trace
	fstl.hb.trace = trace
	fstl.rl = rl
	fstl.dbPrefixes = dbPrefixes
	fstl.itemPresent = false
	if fstl.trace {
		fmt.Printf("----------\n")
		fmt.Printf("RebuildTrie\n")
	}
	if fstl.trace {
		fmt.Printf("fstl.rl: %s\n", fstl.rl)
		fmt.Printf("fixedbits: %d\n", fixedbits)
		fmt.Printf("dbPrefixes(%d): %x\n", len(dbPrefixes), dbPrefixes)
	}
	if len(dbPrefixes) == 0 {
		return nil
	}
	if hasBolt, ok := db.(ethdb.HasKV); ok {
		fstl.boltDB = hasBolt.KV()
	}
	if fstl.boltDB == nil {
		return fmt.Errorf("only Bolt supported yet, given: %T", db)
	}
	fixedbytes := make([]int, len(fixedbits))
	masks := make([]byte, len(fixedbits))
	cutoffs := make([]int, len(fixedbits))
	for i, bits := range fixedbits {
		if bits >= 256 /* addrHash */ +64 /* incarnation */ {
			cutoffs[i] = bits/4 - 16 // Remove incarnation
		} else {
			cutoffs[i] = bits / 4
		}
		fixedbytes[i], masks[i] = ethdb.Bytesmask(bits)
	}
	fstl.fixedbytes = fixedbytes
	fstl.masks = masks
	fstl.cutoffs = cutoffs

	return nil
}

// iteration moves through the database buckets and creates at most
// one stream item, which is indicated by setting the field fstl.itemPresent to true
func (fstl *FlatDbSubTrieLoader) iteration(c, ih *bolt.Cursor, first bool) error {
	var isIH bool
	var minKey []byte
	if !first {
		isIH, minKey = keyIsBefore(fstl.ihK, fstl.k)
	}
	fixedbytes := fstl.fixedbytes[fstl.rangeIdx]
	cutoff := fstl.cutoffs[fstl.rangeIdx]
	dbPrefix := fstl.dbPrefixes[fstl.rangeIdx]
	mask := fstl.masks[fstl.rangeIdx]
	// Adjust rangeIdx if needed
	var cmp int = -1
	for cmp != 0 {
		if minKey == nil {
			if !first {
				cmp = 1
			}
		} else if fixedbytes > 0 { // In the first iteration, we do not have valid minKey, so we skip this part
			if len(minKey) < fixedbytes {
				cmp = bytes.Compare(minKey, dbPrefix[:len(minKey)])
				if cmp == 0 {
					cmp = -1
				}
			} else {
				cmp = bytes.Compare(minKey[:fixedbytes-1], dbPrefix[:fixedbytes-1])
				if cmp == 0 {
					k1 := minKey[fixedbytes-1] & mask
					k2 := dbPrefix[fixedbytes-1] & mask
					if k1 < k2 {
						cmp = -1
					} else if k1 > k2 {
						cmp = 1
					}
				}
			}
		} else {
			cmp = 0
		}
		if cmp == 0 && fstl.itemPresent {
			return nil
		}
		if cmp < 0 {
			// This happens after we have just incremented rangeIdx or on the very first iteration
			if first && len(dbPrefix) > common.HashLength {
				// Looking for storage sub-tree
				copy(fstl.accAddrHashWithInc[:], dbPrefix[:common.HashLength+common.IncarnationLength])
			}
			fstl.k, fstl.v = c.SeekTo(dbPrefix)
			if len(dbPrefix) <= common.HashLength && len(fstl.k) > common.HashLength {
				// Advance past the storage to the first account
				if nextAccount(fstl.k, fstl.nextAccountKey[:]) {
					fstl.k, fstl.v = c.SeekTo(fstl.nextAccountKey[:])
				} else {
					fstl.k = nil
				}
			}
			fstl.ihK, fstl.ihV = ih.SeekTo(dbPrefix)
			if len(dbPrefix) <= common.HashLength && len(fstl.ihK) > common.HashLength {
				// Advance to the first account
				if nextAccount(fstl.ihK, fstl.nextAccountKey[:]) {
					fstl.ihK, fstl.ihV = ih.SeekTo(fstl.nextAccountKey[:])
				} else {
					fstl.ihK = nil
				}
			}
			isIH, minKey = keyIsBefore(fstl.ihK, fstl.k)
			if fixedbytes == 0 {
				cmp = 0
			}
		} else if cmp > 0 {
			if !first {
				fstl.rangeIdx++
			}
			if !first {
				fstl.itemPresent = true
				fstl.itemType = CutoffStreamItem
				fstl.streamCutoff = cutoff
			}
			if fstl.rangeIdx == len(fstl.dbPrefixes) {
				return nil
			}
			fixedbytes = fstl.fixedbytes[fstl.rangeIdx]
			mask = fstl.masks[fstl.rangeIdx]
			dbPrefix = fstl.dbPrefixes[fstl.rangeIdx]
			if len(dbPrefix) > common.HashLength {
				// Looking for storage sub-tree
				copy(fstl.accAddrHashWithInc[:], dbPrefix[:common.HashLength+common.IncarnationLength])
			}
			cutoff = fstl.cutoffs[fstl.rangeIdx]
		}
	}

	if !isIH {
		if len(fstl.k) > common.HashLength && !bytes.HasPrefix(fstl.k, fstl.accAddrHashWithInc[:]) {
			if bytes.Compare(fstl.k, fstl.accAddrHashWithInc[:]) < 0 {
				// Skip all the irrelevant storage in the middle
				fstl.k, fstl.v = c.SeekTo(fstl.accAddrHashWithInc[:])
			} else {
				if nextAccount(fstl.k, fstl.nextAccountKey[:]) {
					fstl.k, fstl.v = c.SeekTo(fstl.nextAccountKey[:])
				} else {
					fstl.k = nil
				}
			}
			return nil
		}
		fstl.itemPresent = true
		if len(fstl.k) > common.HashLength {
			fstl.itemType = StorageStreamItem
			if len(fstl.k) >= common.HashLength {
				fstl.storageKeyPart1 = fstl.k[:common.HashLength]
				if len(fstl.k) >= common.HashLength+common.IncarnationLength {
					fstl.storageKeyPart2 = fstl.k[common.HashLength+common.IncarnationLength:]
				} else {
					fstl.storageKeyPart2 = nil
				}
			} else {
				fstl.storageKeyPart1 = fstl.k
				fstl.storageKeyPart2 = nil
			}
			fstl.storageHash = nil
			fstl.storageValue = fstl.v
			fstl.k, fstl.v = c.Next()
			if fstl.trace {
				fmt.Printf("k after storageWalker and Next: %x\n", fstl.k)
			}
		} else {
			fstl.itemType = AccountStreamItem
			fstl.accountKey = fstl.k
			fstl.accountHash = nil
			if err := fstl.accountValue.DecodeForStorage(fstl.v); err != nil {
				return fmt.Errorf("fail DecodeForStorage: %w", err)
			}
			copy(fstl.accAddrHashWithInc[:], fstl.k)
			binary.BigEndian.PutUint64(fstl.accAddrHashWithInc[32:], ^fstl.accountValue.Incarnation)
			// Now we know the correct incarnation of the account, and we can skip all irrelevant storage records
			// Since 0 incarnation if 0xfff...fff, and we do not expect any records like that, this automatically
			// skips over all storage items
			fstl.k, fstl.v = c.SeekTo(fstl.accAddrHashWithInc[:])
			if fstl.trace {
				fmt.Printf("k after accountWalker and SeekTo: %x\n", fstl.k)
			}
			if !bytes.HasPrefix(fstl.ihK, fstl.accAddrHashWithInc[:]) {
				fstl.ihK, fstl.ihV = ih.SeekTo(fstl.accAddrHashWithInc[:])
			}
		}
		return nil
	}

	// ih part
	fstl.minKeyAsNibbles.Reset()
	keyToNibblesWithoutInc(minKey, &fstl.minKeyAsNibbles)

	if fstl.minKeyAsNibbles.Len() < cutoff {
		fstl.ihK, fstl.ihV = ih.Next() // go to children, not to sibling
		return nil
	}

	retain := fstl.rl.Retain(fstl.minKeyAsNibbles.Bytes())
	if fstl.trace {
		fmt.Printf("fstl.rl.Retain(%x)=%t\n", fstl.minKeyAsNibbles.Bytes(), retain)
	}

	if retain { // can't use ih as is, need go to children
		fstl.ihK, fstl.ihV = ih.Next() // go to children, not to sibling
		return nil
	}

	if len(fstl.ihK) > common.HashLength && !bytes.HasPrefix(fstl.ihK, fstl.accAddrHashWithInc[:]) {
		if bytes.Compare(fstl.ihK, fstl.accAddrHashWithInc[:]) < 0 {
			// Skip all the irrelevant storage in the middle
			fstl.ihK, fstl.ihV = ih.SeekTo(fstl.accAddrHashWithInc[:])
		} else {
			if nextAccount(fstl.ihK, fstl.nextAccountKey[:]) {
				fstl.ihK, fstl.ihV = ih.SeekTo(fstl.nextAccountKey[:])
			} else {
				fstl.ihK = nil
			}
		}
		return nil
	}
	fstl.itemPresent = true
	if len(fstl.ihK) > common.HashLength {
		fstl.itemType = SHashStreamItem
		if len(fstl.ihK) >= common.HashLength {
			fstl.storageKeyPart1 = fstl.ihK[:common.HashLength]
			if len(fstl.ihK) >= common.HashLength+common.IncarnationLength {
				fstl.storageKeyPart2 = fstl.ihK[common.HashLength+common.IncarnationLength:]
			} else {
				fstl.storageKeyPart2 = nil
			}
		} else {
			fstl.storageKeyPart1 = fstl.ihK
			fstl.storageKeyPart2 = nil
		}
		fstl.storageHash = fstl.ihV
		fstl.storageValue = nil
		fstl.storageWitnessLen = fstl.getWitnessLen(fstl.ihK)
	} else {
		fstl.itemType = AHashStreamItem
		fstl.accountKey = fstl.ihK
		fstl.accountHash = fstl.ihV
		fstl.accountWitnessLen = fstl.getWitnessLen(fstl.ihK)
	}

	// skip subtree
	next, ok := nextSubtree(fstl.ihK)
	if !ok { // no siblings left
		if !retain { // last sub-tree was taken from IH, then nothing to look in the main bucket. Can stop.
			fstl.k = nil
			fstl.ihK = nil
			return nil
		}
		fstl.ihK, fstl.ihV = nil, nil
		return nil
	}
	if fstl.trace {
		fmt.Printf("next: %x\n", next)
	}

	if !bytes.HasPrefix(fstl.k, next) {
		fstl.k, fstl.v = c.SeekTo(next)
	}
	if len(next) <= common.HashLength && len(fstl.k) > common.HashLength {
		// Advance past the storage to the first account
		if nextAccount(fstl.k, fstl.nextAccountKey[:]) {
			fstl.k, fstl.v = c.SeekTo(fstl.nextAccountKey[:])
		} else {
			fstl.k = nil
		}
	}
	if fstl.trace {
		fmt.Printf("k after next: %x\n", fstl.k)
	}
	if !bytes.HasPrefix(fstl.ihK, next) {
		fstl.ihK, fstl.ihV = ih.SeekTo(next)
	}
	if len(next) <= common.HashLength && len(fstl.ihK) > common.HashLength {
		// Advance past the storage to the first account
		if nextAccount(fstl.ihK, fstl.nextAccountKey[:]) {
			fstl.ihK, fstl.ihV = ih.SeekTo(fstl.nextAccountKey[:])
		} else {
			fstl.ihK = nil
		}
	}
	if fstl.trace {
		fmt.Printf("ihK after next: %x\n", fstl.ihK)
	}
	return nil
}

// cutoff specifies how many nibbles have to be cut from the beginning of the storage keys
// to fit the insertion point.
// We create a fake branch node at the cutoff point by modifying the last
// nibble of the `succ` key
func (fstl *FlatDbSubTrieLoader) finaliseRoot(cutoff int) error {
	if fstl.trace {
		fmt.Printf("finaliseRoot(%d)\n", cutoff)
	}
	if cutoff >= 2*common.HashLength {
		// if only storage resolution required, then no account records
		if ok, err := fstl.finaliseStorageRoot(cutoff); err == nil {
			if ok {
				fstl.subTries.roots = append(fstl.subTries.roots, fstl.hb.root())
				fstl.subTries.Hashes = append(fstl.subTries.Hashes, fstl.hb.rootHash())
			} else {
				fstl.subTries.roots = append(fstl.subTries.roots, nil)
				fstl.subTries.Hashes = append(fstl.subTries.Hashes, common.Hash{})
			}
		} else {
			return err
		}
		return nil
	}
	fstl.curr.Reset()
	fstl.curr.Write(fstl.succ.Bytes())
	fstl.succ.Reset()
	if fstl.curr.Len() > 0 {
		if cutoff > 0 {
			fstl.succ.Write(fstl.curr.Bytes()[:cutoff-1])
			fstl.succ.WriteByte(fstl.curr.Bytes()[cutoff-1] + 1) // Modify last nibble before the cutoff point
		}
		var data GenStructStepData
		if fstl.wasIH {
			fstl.hashData.Hash = common.BytesToHash(fstl.value.Bytes())
			fstl.hashData.DataLen = fstl.accountWitnessLen
			data = &fstl.hashData
		} else {
			if ok, err := fstl.finaliseStorageRoot(2 * common.HashLength); err == nil {
				if ok {
					// There are some storage items
					fstl.accData.FieldSet |= AccountFieldStorageOnly
				}
			} else {
				return err
			}
			fstl.wasIHStorage = false
			fstl.accData.Balance.Set(&fstl.a.Balance)
			if fstl.a.Balance.Sign() != 0 {
				fstl.accData.FieldSet |= AccountFieldBalanceOnly
			}
			fstl.accData.Nonce = fstl.a.Nonce
			if fstl.a.Nonce != 0 {
				fstl.accData.FieldSet |= AccountFieldNonceOnly
			}
			fstl.accData.Incarnation = fstl.a.Incarnation
			data = &fstl.accData
		}
		var err error
		if fstl.groups, err = GenStructStep(fstl.rl.Retain, fstl.curr.Bytes(), fstl.succ.Bytes(), fstl.hb, data, fstl.groups, false); err != nil {
			return err
		}
		if len(fstl.groups) > cutoff {
			fstl.groups = fstl.groups[:cutoff]
		}
		for len(fstl.groups) > 0 && fstl.groups[len(fstl.groups)-1] == 0 {
			fstl.groups = fstl.groups[:len(fstl.groups)-1]
		}
		fstl.accData.FieldSet = 0
	}
	fstl.subTries.roots = append(fstl.subTries.roots, fstl.hb.root())
	fstl.subTries.Hashes = append(fstl.subTries.Hashes, fstl.hb.rootHash())
	fstl.groups = fstl.groups[:0]
	fstl.hb.Reset()
	fstl.wasIH = false
	fstl.wasIHStorage = false
	fstl.curr.Reset()
	fstl.succ.Reset()
	fstl.currStorage.Reset()
	fstl.succStorage.Reset()
	return nil
}

// cutoff specifies how many nibbles have to be cut from the beginning of the storage keys
// to fit the insertion point. For entire storage subtrie of an account, the cutoff
// would be the the length (in nibbles) of adress Hash.
// For resolver requests that asks for a part of some
// contract's storage, cutoff point will be deeper than that.
// We create a fake branch node at the cutoff point by modifying the last
// nibble of the `succ` key
func (fstl *FlatDbSubTrieLoader) finaliseStorageRoot(cutoff int) (bool, error) {
	if fstl.trace {
		fmt.Printf("finaliseStorageRoot(%d), succStorage.Len() = %d\n", cutoff, fstl.succStorage.Len())
	}
	fstl.currStorage.Reset()
	fstl.currStorage.Write(fstl.succStorage.Bytes())
	fstl.succStorage.Reset()
	if fstl.currStorage.Len() > 0 {
		fstl.succStorage.Write(fstl.currStorage.Bytes()[:cutoff-1])
		fstl.succStorage.WriteByte(fstl.currStorage.Bytes()[cutoff-1] + 1) // Modify last nibble in the incarnation part of the `currStorage`
		var data GenStructStepData
		if fstl.wasIHStorage {
			fstl.hashData.Hash = common.BytesToHash(fstl.valueStorage.Bytes())
			fstl.hashData.DataLen = fstl.storageWitnessLen
			data = &fstl.hashData
		} else {
			fstl.leafData.Value = rlphacks.RlpSerializableBytes(fstl.valueStorage.Bytes())
			data = &fstl.leafData
		}
		var err error
		fstl.groups, err = GenStructStep(fstl.rl.Retain, fstl.currStorage.Bytes(), fstl.succStorage.Bytes(), fstl.hb, data, fstl.groups, false)
		if err != nil {
			return false, err
		}
		if len(fstl.groups) >= cutoff {
			fstl.groups = fstl.groups[:cutoff-1]
		}
		for len(fstl.groups) > 0 && fstl.groups[len(fstl.groups)-1] == 0 {
			fstl.groups = fstl.groups[:len(fstl.groups)-1]
		}
		fstl.currStorage.Reset()
		fstl.succStorage.Reset()
		fstl.wasIHStorage = false
		if fstl.trace {
			fmt.Printf("storage root: %x\n", fstl.hb.rootHash())
		}
		return true, nil
	}
	return false, nil
}

func (fstl *FlatDbSubTrieLoader) LoadSubTries() (SubTries, error) {
	defer trieFlatDbSubTrieLoaderTimer.UpdateSince(time.Now())
	if len(fstl.dbPrefixes) == 0 {
		return SubTries{}, nil
	}
	if err := fstl.boltDB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(dbutils.CurrentStateBucket).Cursor()
		ih := tx.Bucket(dbutils.IntermediateTrieHashBucket).Cursor()
		iwl := tx.Bucket(dbutils.IntermediateTrieWitnessLenBucket).Cursor()
		fstl.getWitnessLen = func(prefix []byte) uint64 {
			if !debug.IsTrackWitnessSizeEnabled() {
				return 0
			}
			k, v := iwl.SeekTo(prefix)
			if !bytes.Equal(k, prefix) {
				panic(fmt.Sprintf("IH and DataLen buckets must have same keys set: %x, %x", k, prefix))
			}
			return binary.BigEndian.Uint64(v)
		}
		if err := fstl.iteration(c, ih, true /* first */); err != nil {
			return err
		}
		for fstl.rangeIdx < len(fstl.dbPrefixes) {
			for !fstl.itemPresent {
				if err := fstl.iteration(c, ih, false /* first */); err != nil {
					return err
				}
			}
			if fstl.itemPresent {
				switch fstl.itemType {
				case StorageStreamItem:
					if err := fstl.WalkerStorage(false, fstl.rangeIdx, fstl.storageKeyPart1, fstl.storageKeyPart2, fstl.storageValue, fstl.storageHash, fstl.storageWitnessLen); err != nil {
						return err
					}
				case SHashStreamItem:
					if err := fstl.WalkerStorage(true, fstl.rangeIdx, fstl.storageKeyPart1, fstl.storageKeyPart2, fstl.storageValue, fstl.storageHash, fstl.storageWitnessLen); err != nil {
						return err
					}
				case AccountStreamItem:
					if err := fstl.WalkerAccount(false, fstl.rangeIdx, fstl.accountKey, &fstl.accountValue, fstl.accountHash, fstl.accountWitnessLen); err != nil {
						return err
					}
				case AHashStreamItem:
					if err := fstl.WalkerAccount(true, fstl.rangeIdx, fstl.accountKey, &fstl.accountValue, fstl.accountHash, fstl.accountWitnessLen); err != nil {
						return err
					}
				case CutoffStreamItem:
					if err := fstl.finaliseRoot(fstl.streamCutoff); err != nil {
						return err
					}
				}
				fstl.itemPresent = false
			}
		}
		return nil
	}); err != nil {
		return fstl.subTries, err
	}
	return fstl.subTries, nil
}

func (fstl *FlatDbSubTrieLoader) AttachRequestedCode(db ethdb.Getter, requests []*LoadRequestForCode) error {
	for _, req := range requests {
		codeHash := req.codeHash
		code, err := db.Get(dbutils.CodeBucket, codeHash[:])
		if err != nil {
			return err
		}
		if req.bytecode {
			if err := req.t.UpdateAccountCode(req.addrHash[:], codeNode(code)); err != nil {
				return err
			}
		} else {
			if err := req.t.UpdateAccountCodeSize(req.addrHash[:], len(code)); err != nil {
				return err
			}
		}
	}
	return nil
}

func keyToNibbles(k []byte, w io.ByteWriter) {
	for _, b := range k {
		//nolint:errcheck
		w.WriteByte(b / 16)
		//nolint:errcheck
		w.WriteByte(b % 16)
	}
}

func keyToNibblesWithoutInc(k []byte, w io.ByteWriter) {
	// Transform k to nibbles, but skip the incarnation part in the middle
	for i, b := range k {
		if i == common.HashLength {
			break
		}
		//nolint:errcheck
		w.WriteByte(b / 16)
		//nolint:errcheck
		w.WriteByte(b % 16)
	}
	if len(k) > common.HashLength+common.IncarnationLength {
		for _, b := range k[common.HashLength+common.IncarnationLength:] {
			//nolint:errcheck
			w.WriteByte(b / 16)
			//nolint:errcheck
			w.WriteByte(b % 16)
		}
	}
}

func (fstl *FlatDbSubTrieLoader) WalkerStorage(isIH bool, rangeIdx int, kPart1, kPart2, v, h []byte, witnessLen uint64) error {
	if fstl.trace {
		fmt.Printf("WalkerStorage: isIH=%v rangeIdx=%d keyPart1=%x keyPart2=%x value=%x hash=%x\n", isIH, rangeIdx, kPart1, kPart2, v, h)
	}

	fstl.currStorage.Reset()
	fstl.currStorage.Write(fstl.succStorage.Bytes())
	fstl.succStorage.Reset()
	// Transform k to nibbles, but skip the incarnation part in the middle
	keyToNibbles(kPart1, &fstl.succStorage)
	keyToNibbles(kPart2, &fstl.succStorage)

	if !isIH {
		fstl.succStorage.WriteByte(16)
	}

	if fstl.currStorage.Len() > 0 {
		var err error
		var data GenStructStepData
		if fstl.trace {
			fmt.Printf("fstl.wasIHStorage=%t\n", fstl.wasIHStorage)
		}
		if fstl.wasIHStorage {
			fstl.hashData.Hash = common.BytesToHash(fstl.valueStorage.Bytes())
			fstl.hashData.DataLen = fstl.witnessLenStorage
			data = &fstl.hashData
		} else {
			fstl.leafData.Value = rlphacks.RlpSerializableBytes(fstl.valueStorage.Bytes())
			data = &fstl.leafData
		}
		fstl.groups, err = GenStructStep(fstl.rl.Retain, fstl.currStorage.Bytes(), fstl.succStorage.Bytes(), fstl.hb, data, fstl.groups, false)
		if err != nil {
			return err
		}
	}
	// Remember the current key and value
	fstl.wasIHStorage = isIH
	fstl.valueStorage.Reset()
	if isIH {
		fstl.valueStorage.Write(h)
		fstl.witnessLenStorage = witnessLen
	} else {
		fstl.valueStorage.Write(v)
	}

	return nil
}

// Walker - k, v - shouldn't be reused in the caller's code
func (fstl *FlatDbSubTrieLoader) WalkerAccount(isIH bool, rangeIdx int, k []byte, v *accounts.Account, h []byte, witnessLen uint64) error {
	if fstl.trace {
		fmt.Printf("WalkerAccount: isIH=%v rangeIdx=%d key=%x hash=%x\n", isIH, rangeIdx, k, h)
	}
	fstl.curr.Reset()
	fstl.curr.Write(fstl.succ.Bytes())
	fstl.succ.Reset()
	for _, b := range k {
		fstl.succ.WriteByte(b / 16)
		fstl.succ.WriteByte(b % 16)
	}
	if !isIH {
		fstl.succ.WriteByte(16)
	}

	if fstl.curr.Len() > 0 {
		var data GenStructStepData
		if fstl.wasIH {
			copy(fstl.hashData.Hash[:], fstl.value.Bytes())
			fstl.hashData.DataLen = fstl.witnessLenAccount
			data = &fstl.hashData
		} else {
			if ok, err := fstl.finaliseStorageRoot(2 * common.HashLength); err == nil {
				if ok {
					// There are some storage items
					fstl.accData.FieldSet |= AccountFieldStorageOnly
				}
			} else {
				return err
			}
			fstl.accData.Balance.Set(&fstl.a.Balance)
			if fstl.a.Balance.Sign() != 0 {
				fstl.accData.FieldSet |= AccountFieldBalanceOnly
			}
			fstl.accData.Nonce = fstl.a.Nonce
			if fstl.a.Nonce != 0 {
				fstl.accData.FieldSet |= AccountFieldNonceOnly
			}
			fstl.accData.Incarnation = fstl.a.Incarnation
			data = &fstl.accData
		}
		fstl.wasIHStorage = false
		fstl.currStorage.Reset()
		fstl.succStorage.Reset()
		var err error
		if fstl.groups, err = GenStructStep(fstl.rl.Retain, fstl.curr.Bytes(), fstl.succ.Bytes(), fstl.hb, data, fstl.groups, false); err != nil {
			return err
		}
		fstl.accData.FieldSet = 0
	}
	// Remember the current key and value
	fstl.wasIH = isIH

	if isIH {
		fstl.value.Reset()
		fstl.value.Write(h)
		fstl.witnessLenAccount = witnessLen
		return nil
	}

	fstl.a.Copy(v)
	// Place code on the stack first, the storage will follow
	if !fstl.a.IsEmptyCodeHash() {
		// the first item ends up deepest on the stack, the second item - on the top
		fstl.accData.FieldSet |= AccountFieldCodeOnly
		if err := fstl.hb.hash(fstl.a.CodeHash[:], 0); err != nil {
			return err
		}
	}
	return nil
}

// nextSubtree does []byte++. Returns false if overflow.
func nextSubtree(in []byte) ([]byte, bool) {
	r := make([]byte, len(in))
	copy(r, in)
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] != 255 {
			r[i]++
			return r, true
		}

		r[i] = 0
	}
	return nil, false
}

func nextAccount(in, out []byte) bool {
	copy(out, in)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 255 {
			out[i]++
			return true
		}
		out[i] = 0
	}
	return false
}

// keyIsBefore - kind of bytes.Compare, but nil is the last key. And return
func keyIsBefore(k1, k2 []byte) (bool, []byte) {
	if k1 == nil {
		return false, k2
	}

	if k2 == nil {
		return true, k1
	}

	switch bytes.Compare(k1, k2) {
	case -1, 0:
		return true, k1
	default:
		return false, k2
	}
}
