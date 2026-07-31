package main

import "strings"

type TrieNode struct {
	children      map[string]*TrieNode
	Upstream      string
	HeaderRewrite map[string]string
}

type DomainRouter struct {
	root *TrieNode
}

func NewDomainRouter() *DomainRouter {
	return &DomainRouter{
		root: &TrieNode{children: make(map[string]*TrieNode)},
	}
}

func (r *DomainRouter) AddRule(domain string, upstream string, rewrite map[string]string) {
	domain = strings.TrimSpace(domain)
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return
	}

	parts := strings.Split(domain, ".")
	node := r.root

	for i := len(parts) - 1; i >= 0; i-- {
		part := parts[i]
		if node.children == nil {
			node.children = make(map[string]*TrieNode)
		}
		if node.children[part] == nil {
			node.children[part] = &TrieNode{}
		}
		node = node.children[part]
	}
	node.Upstream = upstream
	node.HeaderRewrite = rewrite
}

// MatchNode 采用零堆分配的从右向左索引切分匹配算法
func (r *DomainRouter) MatchNode(domain string) *TrieNode {
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return nil
	}

	node := r.root
	var lastMatchedNode *TrieNode
	if node.Upstream != "" {
		lastMatchedNode = node
	}

	end := len(domain)
	for end > 0 {
		start := strings.LastIndexByte(domain[:end], '.')
		var part string
		if start == -1 {
			part = domain[:end]
			end = 0
		} else {
			part = domain[start+1 : end]
			end = start
		}

		child, ok := node.children[part]
		if !ok {
			break
		}
		node = child
		if node.Upstream != "" {
			lastMatchedNode = node
		}
	}

	return lastMatchedNode
}
