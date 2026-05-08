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

func (r *DomainRouter) MatchNode(domain string) *TrieNode {
	domain = strings.TrimSuffix(domain, ".")
	node := r.root
	parts := strings.Split(domain, ".")

	var lastMatchedNode *TrieNode
	if node.Upstream != "" {
		lastMatchedNode = node
	}

	for i := len(parts) - 1; i >= 0; i-- {
		child, ok := node.children[parts[i]]
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
