// Copyright 2022 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package proof contains helpers for constructing log Merkle tree proofs.
package proof

import (
	"fmt"
	"math/bits"

	"github.com/transparency-dev/merkle/compact"
)

// Nodes contains information on how to construct a log Merkle tree proof. It
// supports any proof that has at most one ephemeral node, such as inclusion
// and consistency proofs defined in RFC 6962.
type Nodes struct {
	// IDs contains the IDs of non-ephemeral nodes sufficient to build the proof.
	// If an ephemeral node is needed for a proof, it can be recomputed based on
	// a subset of nodes in this list.
	IDs []compact.NodeID
	// begin is the beginning index (inclusive) into the IDs[begin:end] subslice
	// of the nodes which will be used to re-create the ephemeral node.
	begin int
	// end is the ending (exclusive) index into the IDs[begin:end] subslice of
	// the nodes which will be used to re-create the ephemeral node.
	end int
}

// Inclusion returns the information on how to fetch and construct an inclusion
// proof for the given leaf index in a log Merkle tree of the given size. It
// requires 0 <= index < size.
func Inclusion(index, size uint64) (Nodes, error) {
	if index >= size {
		return Nodes{}, fmt.Errorf("index %d out of bounds for tree size %d", index, size)
	}
	return nodes(index, 0, size), nil
}

// Consistency returns the information on how to fetch and construct a
// consistency proof between the two given tree sizes of a log Merkle tree. It
// requires 0 <= size1 <= size2.
func Consistency(size1, size2 uint64) (Nodes, error) {
	if size1 > size2 {
		return Nodes{}, fmt.Errorf("tree size %d > %d", size1, size2)
	}
	if size1 == size2 || size1 == 0 {
		return Nodes{IDs: []compact.NodeID{}}, nil
	}

	// TODO(pavelkalinnikov): Make the capacity estimate accurate.
	proof := make([]compact.NodeID, 0, bits.Len64(size2)+1)

	// Find the biggest perfect subtree that ends at size1.
	level := uint(bits.TrailingZeros64(size1))
	index := (size1 - 1) >> level
	// If it does not cover the whole size1 tree, add this node to the proof.
	if index != 0 {
		proof = append(proof, compact.NewNodeID(level, index))
	}

	// Now append the path from this node to the root of size2.
	p := nodes(index, level, size2)
	p.IDs = append(proof, p.IDs...)
	// Adjust for the case above when we already put one node in the proof. This
	// happens when size1 is not a power of two, and the verifier needs to be
	// able to re-create the root hash at size1.
	if len(proof) == 1 && p.begin < p.end {
		p.begin++
		p.end++
	}
	return p, nil
}

// nodes returns the node IDs necessary to prove that the (level, index) node
// is included in the Merkle tree of the given size.
func nodes(index uint64, level uint, size uint64) Nodes {
	// [begin, end) is the leaf range covered by the (level, index) node.
	begin, end := index<<level, (index+1)<<level
	// To prove inclusion of range [begin, end), we only need nodes of compact
	// range [0, begin) and [end, size). Further down, we need the nodes ordered
	// by level from leaves towards the root.
	left := reverse(compact.RangeNodes(0, begin))
	// We decompose the [end, size) range into [end, end+l) and [end+l, size).
	// The first one (named `middle` here) contains all the nodes that don't have
	// a left sibling within [end, size), and the second one (named `right`
	// below) contains all the nodes that don't have a right sibling.
	l, _ := compact.Decompose(end, size)
	middle := compact.RangeNodes(end, end+l)
	// Nodes that don't have a right sibling (i.e. the right border of the tree)
	// are special, because their hashes are collapsed into a single "ephemeral"
	// hash. It can be derived from the hashes of compact range [end+l, size).
	right := reverse(compact.RangeNodes(end+l, size))

	// The level in the ordered list of nodes where the rehashed nodes appear in
	// lieu of the "ephemeral" node. This is equal to the level where the path to
	// the `begin` index diverges from the path to `size`.
	rehashLevel := uint(bits.Len64(begin^size) - 1)
	var rehashBegin, rehashEnd int

	// Merge the three compact ranges into a single proof ordered by node level
	// from leaves towards the root, i.e. the format specified in RFC 6962.
	proof := make([]compact.NodeID, 0, len(left)+len(middle)+len(right))
	i, j := 0, 0
	for l, levels := level, uint(bits.Len64(size-1)); l < levels; l++ {
		if i < len(left) && left[i].Level == l {
			proof = append(proof, left[i])
			i++
		} else if j < len(middle) && middle[j].Level == l {
			proof = append(proof, middle[j])
			j++
		}
		if l == rehashLevel {
			proof = append(proof, right...)
			if len(right) > 1 {
				rehashBegin = len(proof) - len(right)
				rehashEnd = len(proof)
			}
		}
	}

	return Nodes{IDs: proof, begin: rehashBegin, end: rehashEnd}
}

// Rehash computes the proof based on the slice of node hashes corresponding to
// their IDs in the n.IDs field. The slices must be of the same length. The hc
// parameter computes a node's hash based on hashes of its children.
//
// Warning: The passed-in slice of hashes can be modified in-place.
func (n Nodes) Rehash(h [][]byte, hc func(left, right []byte) []byte) ([][]byte, error) {
	if got, want := len(h), len(n.IDs); got != want {
		return nil, fmt.Errorf("got %d hashes but expected %d", got, want)
	}
	cursor := 0
	// Scan the list of node hashes, and store the rehashed list in-place.
	// Invariant: cursor <= i, and h[:cursor] contains all the hashes of the
	// rehashed list after scanning h up to index i-1.
	for i, ln := 0, len(h); i < ln; i, cursor = i+1, cursor+1 {
		hash := h[i]
		if i >= n.begin && i < n.end {
			// Scan the block of node hashes that need rehashing.
			for i++; i < n.end; i++ {
				hash = hc(h[i], hash)
			}
			i--
		}
		h[cursor] = hash
	}
	return h[:cursor], nil
}

func reverse(ids []compact.NodeID) []compact.NodeID {
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
	return ids
}
