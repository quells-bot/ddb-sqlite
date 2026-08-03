package attrval

import "sort"

// Project returns a copy of item containing only the values at the given
// paths, preserving document spines. Paths that do not resolve (missing
// attribute, index out of range, or descending into a non-container) are
// omitted — no error — and containers left empty by pruning are omitted too.
// Overlapping paths must be rejected by the caller before calling Project
// (expr.BoundProjection.Bind does this). Only the touched spine is copied;
// the receiver is unchanged.
//
// List spines merge by source index, not append order: paths converging on
// the same index of the same list land in ONE result element
// ("arr[1].x, arr[1].y" -> [{"x":…,"y":…}]), and result lists are compacted
// — gaps are not preserved ("arr[1]" on a 3-element list -> [elem]).
func Project(item map[string]Value, paths []Path) map[string]Value {
	root := &projNode{children: map[string]*projNode{}}
	for _, p := range paths {
		if len(p) == 0 || p[0].IsIndex {
			continue // the expression parser never produces these
		}
		v, ok := item[p[0].Name]
		if !ok {
			continue
		}
		child := root.children[p[0].Name]
		if child == nil {
			child = &projNode{}
			root.children[p[0].Name] = child
		}
		child.merge(v, p[1:])
	}
	out := make(map[string]Value, len(root.children))
	for name, n := range root.children {
		if v, ok := n.value(); ok {
			out[name] = v
		}
	}
	return out
}

// projNode is one mutable spine node under construction. Exactly one of the
// three shapes is meaningful: a leaf (the value copied from the item), a map
// spine (children), or a list spine (slots).
type projNode struct {
	leaf     Value
	isLeaf   bool
	children map[string]*projNode
	slots    []projSlot
}

// projSlot is one compacted result-list element, remembering the source
// index it came from so convergent paths merge into one slot.
type projSlot struct {
	src  int
	node *projNode
}

// merge inserts the subtree of src addressed by p into n. A path that does
// not resolve inserts nothing.
func (n *projNode) merge(src Value, p Path) {
	if len(p) == 0 {
		n.leaf, n.isLeaf = src, true
		n.children, n.slots = nil, nil
		return
	}
	seg := p[0]
	if seg.IsIndex {
		if src.tag != TagList || seg.Index < 0 || seg.Index >= len(src.list) {
			return
		}
		child := n.slotFor(seg.Index)
		child.merge(src.list[seg.Index], p[1:])
		return
	}
	if src.tag != TagMap {
		return
	}
	v, ok := src.m[seg.Name]
	if !ok {
		return
	}
	if n.children == nil {
		n.children = map[string]*projNode{}
	}
	child := n.children[seg.Name]
	if child == nil {
		child = &projNode{}
		n.children[seg.Name] = child
	}
	child.merge(v, p[1:])
}

// slotFor finds or creates the slot for srcIndex, appending in first-seen
// (path) order. Compaction falls out: slots exist only for seen indices.
func (n *projNode) slotFor(srcIndex int) *projNode {
	for i := range n.slots {
		if n.slots[i].src == srcIndex {
			return n.slots[i].node
		}
	}
	child := &projNode{}
	n.slots = append(n.slots, projSlot{src: srcIndex, node: child})
	return child
}

// value freezes the node into a Value. The second result is false when the
// node holds nothing (a path that never resolved), so empty spines prune.
func (n *projNode) value() (Value, bool) {
	if n.isLeaf {
		return n.leaf, true
	}
	if n.children != nil {
		m := make(map[string]Value, len(n.children))
		for k, c := range n.children {
			if v, ok := c.value(); ok {
				m[k] = v
			}
		}
		if len(m) == 0 {
			return Value{}, false
		}
		return Value{tag: TagMap, m: m}, true
	}
	if n.slots != nil {
		// Slot order is source-index order (probe-verified, §2.5 probe 24):
		// both "arr[0], arr[2]" and "arr[2], arr[0]" emit [e0, e2].
		sort.SliceStable(n.slots, func(i, j int) bool {
			return n.slots[i].src < n.slots[j].src
		})
		l := make([]Value, 0, len(n.slots))
		for _, s := range n.slots {
			if v, ok := s.node.value(); ok {
				l = append(l, v)
			}
		}
		if len(l) == 0 {
			return Value{}, false
		}
		return Value{tag: TagList, list: l}, true
	}
	return Value{}, false
}
