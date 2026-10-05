package cache

import (
	"net"
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Sender kinds and where a kind came from.
const (
	KindHuman         = "human"
	KindList          = "list"
	KindTransactional = "transactional"
	KindNotification  = "notification"

	SourceRule  = "rule"
	SourceLLM   = "llm"
	SourceOwner = "owner"
)

// SenderKinds is every kind a sender can have.
var SenderKinds = []string{KindHuman, KindList, KindTransactional, KindNotification}

// ValidSenderKind reports whether k is one of SenderKinds.
func ValidSenderKind(k string) bool {
	for _, x := range SenderKinds {
		if x == k {
			return true
		}
	}
	return false
}

// KindInputs is what the rules look at: the sender's address and the counts
// of its messages that carry each signal.
type KindInputs struct {
	Addr, Domain string
	NMsgs        int
	NReplied     int // messages the owner sent in reply to this sender
	NList        int // messages with a List-Id
	NUnsub       int // messages with a List-Unsubscribe
	NGH          int // messages with X-GitHub-Reason
	NTxn         int // messages whose subject has an order/invoice/receipt/shipping shape
}

var (
	noreplyRE = regexp.MustCompile(`(?i)no[-_.]?reply|do[-_.]?not[-_.]?reply`)
	txnSubjRE = regexp.MustCompile(`(?i)\b(order|orders|invoice|receipt|shipping|shipped|shipment|delivery|delivered|payment|paid|refund|tracking|bestelling|factuur|bezorg\w*|betaling|pakket|verzonden|bestellbest\w+|rechnung|commande|facture|livraison)\b`)

	// notifierDomains are registrable domains whose mail is automated
	// notification traffic (CI, tracker, chat, monitoring, compliance).
	notifierDomains = map[string]bool{
		"github.com": true, "githubusercontent.com": true, "gitlab.com": true, "bitbucket.org": true,
		"linear.app": true, "vanta.com": true, "steady.space": true, "betterstack.com": true, "betteruptime.com": true,
		"atlassian.com": true, "atlassian.net": true, "jira.com": true, "slack.com": true, "sentry.io": true,
		"pagerduty.com": true, "opsgenie.com": true, "datadoghq.com": true, "circleci.com": true, "vercel.com": true,
		"netlify.com": true, "notion.so": true, "asana.com": true, "trello.com": true, "figma.com": true,
		"grafana.com": true, "statuspage.io": true, "cloudflare.com": true, "docker.com": true, "npmjs.com": true,
	}
	// shopDomains are registrable domains of shops, carriers and payment
	// processors whose mail is order/payment traffic.
	shopDomains = map[string]bool{
		"bol.com": true, "coolblue.nl": true, "coolblue.be": true, "stripe.com": true, "postnl.nl": true,
		"mollie.com": true, "klarna.com": true, "shopify.com": true, "shopifyemail.com": true,
		"marktplaats.nl": true, "uber.com": true, "booking.com": true, "adyen.com": true, "etsy.com": true,
		"temu.com": true, "alibaba.com": true, "wehkamp.nl": true, "thuisbezorgd.nl": true, "ups.com": true,
		"fedex.com": true, "dhl.com": true, "dhl.de": true, "dpd.com": true, "gls-group.eu": true,
	}
	// shopLabels match a registrable domain by its first label, on any TLD
	// (amazon.nl, amazon.co.uk, ...).
	shopLabels = map[string]bool{
		"amazon": true, "aliexpress": true, "ebay": true, "paypal": true, "zalando": true, "ikea": true,
		"dhl": true, "dpd": true, "ups": true, "airbnb": true,
	}
)

// RegistrableDomain is the registrable domain (eTLD+1, from the public
// suffix list) of the host part of addr; the host itself when it has none
// (an IP, a bare TLD, an unknown suffix with one label).
func RegistrableDomain(addr string) string {
	host := addr
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		host = addr[i+1:]
	}
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), ".[]"))
	if host == "" || net.ParseIP(host) != nil {
		return host
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

func localPart(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[:i]
	}
	return addr
}

func isShop(domain string) bool {
	if shopDomains[domain] {
		return true
	}
	label, _, _ := strings.Cut(domain, ".")
	return shopLabels[label]
}

// IsTransactionalSubject reports whether a subject has an order, invoice,
// receipt, shipping or payment shape.
func IsTransactionalSubject(s string) bool { return txnSubjRE.MatchString(s) }

// ClassifySender applies the rules, in this order, first match wins:
//
//  1. notification: a known notifier domain, or any X-GitHub-Reason
//  2. human: the owner has replied to this sender
//  3. transactional: a known shop, carrier or payment domain
//  4. noreply-style address: transactional when any subject has an order shape,
//     notification when any message carries List-Id, List-Unsubscribe or
//     X-GitHub-Reason (automated headers)
//  5. list: any message carries a List-Id
//  6. human: everything else
//
// The rules see only headers and subjects, never bodies, so they are
// conservative about "human": a marketing sender without List-Id or
// List-Unsubscribe, or a shop on an unlisted domain, reads as human.
func ClassifySender(in KindInputs) string {
	domain := in.Domain
	if domain == "" {
		domain = RegistrableDomain(in.Addr)
	}
	switch {
	case notifierDomains[domain] || in.NGH > 0:
		return KindNotification
	case in.NReplied > 0:
		return KindHuman
	case isShop(domain):
		return KindTransactional
	}
	if noreplyRE.MatchString(localPart(in.Addr)) {
		switch {
		case in.NTxn > 0:
			return KindTransactional
		case in.NList > 0 || in.NUnsub > 0:
			return KindNotification
		}
	}
	if in.NList > 0 {
		return KindList
	}
	return KindHuman
}
