package main

import (
	"net/netip"
	"strings"

	"go.uber.org/zap"
)

type RuleType int

const (
	RuleTypeDomainSuffix RuleType = iota
	RuleTypeDomainKeyword
	RuleTypeDomainFull
	RuleTypeIPCidr
	RuleTypeGeoIP
	RuleTypeMatch
)

type RuleItem struct {
	Type          RuleType
	Payload       string
	Cidr          netip.Prefix
	Proxy         string
	HeaderRewrite map[string]string
}

type RuleEngine struct {
	trie  *DomainRouter
	items []RuleItem
}

var globalRuleEngine *RuleEngine

func NewRuleEngine(ruleConfigs []RuleConfig) *RuleEngine {
	engine := &RuleEngine{
		trie: NewDomainRouter(),
	}

	for _, rCfg := range ruleConfigs {
		rTypeStr := strings.ToUpper(strings.TrimSpace(rCfg.Type))

		if rTypeStr == "" && len(rCfg.Domains) > 0 {
			engine.trie.AddRule("", rCfg.Proxy, rCfg.HeaderRewrite)
			for _, d := range rCfg.Domains {
				engine.trie.AddRule(d, rCfg.Proxy, rCfg.HeaderRewrite)
			}
			continue
		}

		switch rTypeStr {
		case "DOMAIN-SUFFIX", "DOMAIN_SUFFIX", "":
			engine.trie.AddRule(rCfg.Payload, rCfg.Proxy, rCfg.HeaderRewrite)
		case "DOMAIN", "DOMAIN-FULL":
			item := RuleItem{
				Type:          RuleTypeDomainFull,
				Payload:       strings.TrimSuffix(strings.TrimSpace(rCfg.Payload), "."),
				Proxy:         rCfg.Proxy,
				HeaderRewrite: rCfg.HeaderRewrite,
			}
			engine.items = append(engine.items, item)
		case "DOMAIN-KEYWORD":
			item := RuleItem{
				Type:          RuleTypeDomainKeyword,
				Payload:       strings.ToLower(strings.TrimSpace(rCfg.Payload)),
				Proxy:         rCfg.Proxy,
				HeaderRewrite: rCfg.HeaderRewrite,
			}
			engine.items = append(engine.items, item)
		case "IP-CIDR", "IP_CIDR":
			prefix, err := netip.ParsePrefix(strings.TrimSpace(rCfg.Payload))
			if err == nil {
				item := RuleItem{
					Type:          RuleTypeIPCidr,
					Payload:       rCfg.Payload,
					Cidr:          prefix,
					Proxy:         rCfg.Proxy,
					HeaderRewrite: rCfg.HeaderRewrite,
				}
				engine.items = append(engine.items, item)
			}
		case "GEOIP":
			item := RuleItem{
				Type:          RuleTypeGeoIP,
				Payload:       strings.ToUpper(strings.TrimSpace(rCfg.Payload)),
				Proxy:         rCfg.Proxy,
				HeaderRewrite: rCfg.HeaderRewrite,
			}
			engine.items = append(engine.items, item)
		case "MATCH":
			item := RuleItem{
				Type:          RuleTypeMatch,
				Proxy:         rCfg.Proxy,
				HeaderRewrite: rCfg.HeaderRewrite,
			}
			engine.items = append(engine.items, item)
		}
	}

	globalRuleEngine = engine
	return engine
}

func (re *RuleEngine) Match(domain string, targetIP netip.Addr) (string, map[string]string) {
	if re == nil {
		return cfg.Routing.DefaultUpstream, nil
	}

	// 1. Trie Suffix Match
	if domain != "" {
		if node := re.trie.MatchNode(domain); node != nil && node.Upstream != "" {
			zap.S().Debugf("[RuleEngine] Matched DOMAIN-SUFFIX for '%s' -> Proxy: %s", domain, node.Upstream)
			return node.Upstream, node.HeaderRewrite
		}
	}

	// 2. Advanced Rule Items Match
	domainLower := strings.ToLower(domain)
	for _, item := range re.items {
		switch item.Type {
		case RuleTypeDomainFull:
			if domainLower == item.Payload {
				zap.S().Debugf("[RuleEngine] Matched DOMAIN-FULL '%s' -> Proxy: %s", item.Payload, item.Proxy)
				return item.Proxy, item.HeaderRewrite
			}
		case RuleTypeDomainKeyword:
			if strings.Contains(domainLower, item.Payload) {
				zap.S().Debugf("[RuleEngine] Matched DOMAIN-KEYWORD '%s' in '%s' -> Proxy: %s", item.Payload, domain, item.Proxy)
				return item.Proxy, item.HeaderRewrite
			}
		case RuleTypeIPCidr:
			if targetIP.IsValid() && item.Cidr.Contains(targetIP) {
				zap.S().Debugf("[RuleEngine] Matched IP-CIDR %s for IP %s -> Proxy: %s", item.Cidr, targetIP, item.Proxy)
				return item.Proxy, item.HeaderRewrite
			}
		case RuleTypeGeoIP:
			if item.Payload == "PRIVATE" || item.Payload == "LAN" {
				if targetIP.IsValid() && (targetIP.IsPrivate() || targetIP.IsLoopback()) {
					zap.S().Debugf("[RuleEngine] Matched GEOIP PRIVATE/LAN for IP %s -> Proxy: %s", targetIP, item.Proxy)
					return item.Proxy, item.HeaderRewrite
				}
			}
		case RuleTypeMatch:
			zap.S().Debugf("[RuleEngine] Matched MATCH fallback rule -> Proxy: %s", item.Proxy)
			return item.Proxy, item.HeaderRewrite
		}
	}

	zap.S().Debugf("[RuleEngine] No rule matched for '%s' (IP %s), fallback to DefaultUpstream -> %s", domain, targetIP, cfg.Routing.DefaultUpstream)
	return cfg.Routing.DefaultUpstream, nil
}
