// Copyright 2023 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>

package triestate

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie/trienode"
	"golang.org/x/crypto/sha3"
)

// Trie is an Ethereum state trie, can be implemented by Ethereum Merkle Patricia
// tree or Verkle tree.
type Trie interface {
	// Get returns the value for key stored in the trie.
	Get(key []byte) ([]byte, error)

	// Update associates key with value in the trie.
	Update(key, value []byte) error

	// Delete removes any existing value for key from the trie.
	Delete(key []byte) error

	// Commit the trie and returns a set of dirty nodes generated along with
	// the new root hash.
	Commit(collectLeaf bool) (common.Hash, *trienode.NodeSet, error)
}

// TrieLoader wraps functions to load tries.
type TrieLoader interface {
	// OpenTrie opens the main account trie.
	OpenTrie(root common.Hash) (Trie, error)

	// OpenStorageTrie opens the storage trie of an account.
	OpenStorageTrie(stateRoot common.Hash, addrHash, root common.Hash) (Trie, error)
}

// Set represents a collection of mutated states during a state transition.
// The value refers to the original content of state before the transition
// is made. Nil means that the state was not present previously.
type Set struct {
	Accounts   map[common.Address][]byte                 // Mutated account set, nil means the account was not present
	Storages   map[common.Address]map[common.Hash][]byte // Mutated storage set, nil means the slot was not present
	Incomplete map[common.Address]struct{}               // Indicator whether the storage is incomplete due to large deletion
	size       common.StorageSize                        // Approximate size of set
}

// New constructs the state set with provided data.
func New(accounts map[common.Address][]byte, storages map[common.Address]map[common.Hash][]byte, incomplete map[common.Address]struct{}) *Set {
	return &Set{
		Accounts:   accounts,
		Storages:   storages,
		Incomplete: incomplete,
	}
}

// Size returns the approximate memory size occupied by the set.
func (s *Set) Size() common.StorageSize {
	if s.size != 0 {
		return s.size
	}
	for _, account := range s.Accounts {
		s.size += common.StorageSize(common.AddressLength + len(account))
	}
	for _, slots := range s.Storages {
		for _, val := range slots {
			s.size += common.StorageSize(common.HashLength + len(val))
		}
		s.size += common.StorageSize(common.AddressLength)
	}
	s.size += common.StorageSize(common.AddressLength * len(s.Incomplete))
	return s.size
}

// context wraps all fields for executing state diffs.
type context struct {
	prevRoot    common.Hash
	postRoot    common.Hash
	accounts    map[common.Address][]byte
	storages    map[common.Address]map[common.Hash][]byte
	accountTrie Trie
	nodes       *trienode.MergedNodeSet
}

// Apply traverses the provided state diffs, apply them in the associated
// post-state and return the generated dirty trie nodes. The state can be
// loaded via the provided trie loader.
func Apply(prevRoot common.Hash, postRoot common.Hash, accounts map[common.Address][]byte, storages map[common.Address]map[common.Hash][]byte, loader TrieLoader) (map[common.Hash]map[string]*trienode.Node, error) {
	tr, err := loader.OpenTrie(postRoot)
	if err != nil {
		return nil, err
	}
	ctx := &context{
		prevRoot:    prevRoot,
		postRoot:    postRoot,
		accounts:    accounts,
		storages:    storages,
		accountTrie: tr,
		nodes:       trienode.NewMergedNodeSet(),
	}
	for addr, account := range accounts {
		var err error
		if len(account) == 0 {
			err = deleteAccount(ctx, loader, addr)
		} else {
			err = updateAccount(ctx, loader, addr)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to revert state, err: %w", err)
		}
	}
	root, result, err := tr.Commit(false)
	if err != nil {
		return nil, err
	}
	if root != prevRoot {
		return nil, fmt.Errorf("failed to revert state, want %#x, got %#x", prevRoot, root)
	}
	if err := ctx.nodes.Merge(result); err != nil {
		return nil, err
	}
	return ctx.nodes.Flatten(), nil
}

func ApplyForDiff(prevRoot common.Hash, postRoot common.Hash, accounts map[common.Address][]byte, storages map[common.Address]map[common.Hash][]byte,
	loader TrieLoader) (*trienode.MergedNodeSet, error) {
	tr, err := loader.OpenTrie(prevRoot)
	if err != nil {
		log.Error("Failed to open trie", "error", err)
		return nil, err
	}
	ctx := &context{
		prevRoot:    prevRoot,
		postRoot:    postRoot,
		accounts:    accounts,
		storages:    storages,
		accountTrie: tr,
		nodes:       trienode.NewMergedNodeSet(),
	}
	for addr, account := range accounts {
		var err error
		if len(account) == 0 {
			log.Info("18189")
			// continue
			err = deleteAccountForRecovering(ctx, loader, addr)
		} else {
			err = updateAccountForRecovering(ctx, loader, addr)
		}
		// err = updateAccountForRecovering(ctx, loader, addr)
		if err != nil {
			return nil, fmt.Errorf("failed to apply state, err: %w", err)
		}
		log.Info("388234")
	}
	root, result, err := tr.Commit(false)
	log.Info("print", "root", root, "result", result)

	if err != nil {
		return nil, err
	}
	if root != postRoot {
		return nil, fmt.Errorf("failed to revert state, want %#x, got %#x", postRoot, root)
	}
	if err := ctx.nodes.Merge(result); err != nil {
		return nil, err
	}
	return ctx.nodes, nil
}

// updateAccount the account was present in prev-state, and may or may not
// existent in post-state. Apply the reverse diff and verify if the storage
// root matches the one in prev-state account.
func updateAccount(ctx *context, loader TrieLoader, addr common.Address) error {
	// The account was present in prev-state, decode it from the
	// 'slim-rlp' format bytes.
	h := newHasher()
	defer h.release()

	addrHash := h.hash(addr.Bytes())
	prev, err := types.FullAccount(ctx.accounts[addr])
	if err != nil {
		log.Error("1")
		return err
	}
	// The account may or may not existent in post-state, try to
	// load it and decode if it's found.
	blob, err := ctx.accountTrie.Get(addrHash.Bytes())
	if err != nil {
		log.Error("2")
		return err
	}
	post := types.NewEmptyStateAccount()
	if len(blob) != 0 {
		if err := rlp.DecodeBytes(blob, &post); err != nil {
			log.Error("3")
			return err
		}
	}
	// Apply all storage changes into the post-state storage trie.
	st, err := loader.OpenStorageTrie(ctx.postRoot, addrHash, post.Root)
	if err != nil {
		log.Error("4")
		return err
	}
	for key, val := range ctx.storages[addr] {
		var err error
		if len(val) == 0 {
			err = st.Delete(key.Bytes())
		} else {
			err = st.Update(key.Bytes(), val)
		}
		if err != nil {
			log.Error("5")
			return err
		}
	}
	root, result, err := st.Commit(false)
	if err != nil {
		log.Error("6")
		return err
	}
	if root != prev.Root {
		return errors.New("failed to reset storage trie")
	}
	// The returned set can be nil if storage trie is not changed
	// at all.
	if result != nil {
		if err := ctx.nodes.Merge(result); err != nil {
			log.Error("7")
			return err
		}
	}
	// Write the prev-state account into the main trie
	full, err := rlp.EncodeToBytes(prev)
	if err != nil {
		log.Error("8")
		return err
	}
	return ctx.accountTrie.Update(addrHash.Bytes(), full)
}

// deleteAccount the account was not present in prev-state, and is expected
// to be existent in post-state. Apply the reverse diff and verify if the
// account and storage is wiped out correctly.
func deleteAccount(ctx *context, loader TrieLoader, addr common.Address) error {
	// The account must be existent in post-state, load the account.
	h := newHasher()
	defer h.release()

	addrHash := h.hash(addr.Bytes())
	blob, err := ctx.accountTrie.Get(addrHash.Bytes())
	if err != nil {
		log.Error("9")
		return err
	}
	if len(blob) == 0 {
		return fmt.Errorf("account is non-existent %#x", addrHash)
	}
	var post types.StateAccount
	if err := rlp.DecodeBytes(blob, &post); err != nil {
		log.Error("10")
		return err
	}
	st, err := loader.OpenStorageTrie(ctx.postRoot, addrHash, post.Root)
	if err != nil {
		log.Error("11")
		return err
	}
	for key, val := range ctx.storages[addr] {
		if len(val) != 0 {
			return errors.New("expect storage deletion")
		}
		if err := st.Delete(key.Bytes()); err != nil {
			log.Error("12")
			return err
		}
	}
	root, result, err := st.Commit(false)
	if err != nil {
		log.Error("13")
		return err
	}
	if root != types.EmptyRootHash {
		return errors.New("failed to clear storage trie")
	}
	// The returned set can be nil if storage trie is not changed
	// at all.
	if result != nil {
		if err := ctx.nodes.Merge(result); err != nil {
			log.Error("14")
			return err
		}
	}
	// Delete the post-state account from the main trie.
	return ctx.accountTrie.Delete(addrHash.Bytes())
}

// updateAccountForRecovering the account is present in post-state, and may or may not
// be existent in prev-state. Apply the diff and verify if the storage root matches the
// one in post-state account.
func updateAccountForRecovering(ctx *context, loader TrieLoader, addr common.Address) error {
	// The account was present in post-state, decode it from the
	// 'slim-rlp' format bytes.
	h := newHasher()
	defer h.release()

	addrHash := h.hash(addr.Bytes())
	a, ok := ctx.accounts[addr]
	log.Info("uuu", "length", len(a), "addr", addr.String(), "ok", ok)
	post, err := types.FullAccount(ctx.accounts[addr])
	if err != nil {
		log.Error("Failed to full account for updating", "error", err, "addr", addr.String())
		return err
	}
	// The account may or may not be existent in prev-state, try to
	// load it and decode if it's found.
	blob, err := ctx.accountTrie.Get(addrHash.Bytes())
	if err != nil {
		log.Error("Failed to get for updating", "error", err)
		return err
	}
	prev := types.NewEmptyStateAccount()
	if len(blob) != 0 {
		if err = rlp.DecodeBytes(blob, &prev); err != nil {
			log.Error("Failed to decode bytes for updating", "error", err)
			return err
		}
	}
	// Apply all storage changes into the prev-state storage trie
	st, err := loader.OpenStorageTrie(ctx.prevRoot, addrHash, prev.Root)
	if err != nil {
		log.Error("Failed to open storage trie for updating", "error", err)
		return err
	}
	for k, v := range ctx.storages[addr] {
		if len(v) == 0 {
			log.Info("euu")
			err = st.Delete(k.Bytes())
		} else {
			err = st.Update(k.Bytes(), v)
		}
		// err = st.Update(k.Bytes(), v)
		if err != nil {
			log.Error("Failed to delete or update", "error", err)
			return err
		}
	}
	root, result, err := st.Commit(false)
	if err != nil {
		log.Error("Failed to commit for updating", "error", err)
		return err
	}
	if root != post.Root {
		return errors.New("failed to reset storage trie")
	}
	if result != nil {
		if err = ctx.nodes.Merge(result); err != nil {
			log.Error("Failed to merge for updating", "error", err)
			return err
		}
	}
	// Write the post-state account into the main trie
	full, err := rlp.EncodeToBytes(post)
	if err != nil {
		log.Error("Failed to encode bytes", "error", err)
		return err
	}
	return ctx.accountTrie.Update(addrHash.Bytes(), full)
}

// deleteAccountForRecovering the account is not present in post-state, and was expected
// to be existent in prev-state. Apply the diff and verify if the account and storage
// is wiped out correctly.
func deleteAccountForRecovering(ctx *context, loader TrieLoader, addr common.Address) error {
	// The account must be existent in prev-state, load the account
	h := newHasher()
	defer h.release()

	log.Info("2399324")
	addrHash := h.hash(addr.Bytes())
	blob, err := ctx.accountTrie.Get(addrHash.Bytes())
	if err != nil {
		log.Error("wueuw")
		return err
	}
	if len(blob) == 0 {
		log.Info("ewrwee", "addr", addr.String())
		return fmt.Errorf("account is nonexistent %#x", addrHash)
	}
	var prev types.StateAccount
	if err = rlp.DecodeBytes(blob, prev); err != nil {
		log.Error("Failed to decode bytes for deleting accounts", "error", err)
		return err
	}
	st, err := loader.OpenStorageTrie(ctx.prevRoot, addrHash, prev.Root)
	if err != nil {
		log.Error("Failed to open storage trie for del", "error", err)
		return err
	}
	for k, v := range ctx.storages[addr] {
		if len(v) != 0 {
			return errors.New("expect storage deletion")
		}
		if err = st.Delete(k.Bytes()); err != nil {
			log.Error("Failed to delete for del", "error", err)
			return err
		}
	}
	root, result, err := st.Commit(false)
	if err != nil {
		log.Error("Failed to commit for del", "error", err)
		return err
	}
	if root != types.EmptyRootHash {
		return errors.New("failed to clear storage trie")
	}
	if result != nil {
		if err = ctx.nodes.Merge(result); err != nil {
			log.Error("Failed to merge for del", "error", err)
			return err
		}
	}
	// Delete the prev-state account from the main trie.
	return ctx.accountTrie.Delete(addrHash.Bytes())
}

// hasher is used to compute the sha256 hash of the provided data.
type hasher struct{ sha crypto.KeccakState }

var hasherPool = sync.Pool{
	New: func() interface{} { return &hasher{sha: sha3.NewLegacyKeccak256().(crypto.KeccakState)} },
}

func newHasher() *hasher {
	return hasherPool.Get().(*hasher)
}

func (h *hasher) hash(data []byte) common.Hash {
	return crypto.HashData(h.sha, data)
}

func (h *hasher) release() {
	hasherPool.Put(h)
}
