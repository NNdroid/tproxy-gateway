package main

import "strings"

type TrieNode struct {
	children map[string]*TrieNode
	Upstream string
}

type DomainRouter struct {
	root *TrieNode
}

func NewDomainRouter() *DomainRouter {
	return &DomainRouter{
		root: &TrieNode{children: make(map[string]*TrieNode)},
	}
}

func (r *DomainRouter) AddRule(domain string, upstream string) {
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
}

func (r *DomainRouter) Match(domain string) (string, bool) {
	if domain == "" {
		return "", false
	}
	if domain[len(domain)-1] == '.' {
		domain = domain[:len(domain)-1]
	}

	node := r.root
	right := len(domain)

	for i := len(domain) - 1; i >= -1; i-- {
		if i == -1 || domain[i] == '.' {
			part := domain[i+1 : right]
			right = i

			child, ok := node.children[part]
			if !ok {
				if node.Upstream != "" {
					return node.Upstream, true
				}
				return "", false
			}
			node = child
		}
	}

	if node != nil && node.Upstream != "" {
		return node.Upstream, true
	}
	return "", false
}