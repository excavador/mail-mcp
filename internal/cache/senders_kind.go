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
	NAuto        int // messages that look automated: noreply-style sender, List-Id, List-Unsubscribe or X-GitHub-Reason
}

var (
	// noreplyRE matches a no-reply local part delimited by separators or the
	// ends, so "piano.reply" does not match.
	noreplyRE = regexp.MustCompile(`(?i)(^|[._+-])(no[-_.]?reply|do[-_.]?not[-_.]?reply)($|[._+0-9-])`)
	// marketingRE matches a local part that names a bulk campaign stream
	// (newsletter@, promotion5@, store-news@, ae-market.ae3@, ae-newsletter05.a0@, email.campaign@),
	// each word delimited by separators, digits or the ends.
	marketingRE = regexp.MustCompile(`(?i)(^|[._+=-])(newsletters?|news|nieuwsbrief|promo|promos|promotions?|promotional|deals?|market|offers?|offerte|aanbiedingen|marketing|campaigns?|digest|mailings?)($|[._+0-9=-])`)
	txnSubjRE   = regexp.MustCompile(`(?i)\b(order|orders|invoice|receipt|shipping|shipped|shipment|delivery|delivered|payment|paid|refund|tracking|bestell\w*|factuur|bezorg\w*|betaling|pakket|verzonden|unterwegs|versand\w*|rechnung|commande|facture|livraison|booking|reservation|reservering|tickets?|trip|confirmed|confirmation|purchase|bevestiging|levering|bestätigung|boarding)\b`)
	// promoSubjRE marks a subject that sells ("Free shipping on your order",
	// "Delivery deals"): it only counts as order mail with an order number.
	promoSubjRE = regexp.MustCompile(`(?i)\b(free|gratis|deals?|sale|off|discount|korting|coupons?|promo\w*|save|offers?|aanbieding\w*|now|nu|win|new)\b|\d+ ?% ?off`)
	// orderNumRE is order evidence in a subject: #12345, an Amazon 3-7-7 id, "order no".
	orderNumRE = regexp.MustCompile(`(?i)#\d{5,}|\b\d{3}-\d{7}-\d{7}\b|\b(order|bestelling|bestelnummer)\s*(no|nr|number|nummer)\b`)

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
	// The first-label match is for ICANN suffixes only: ups.github.io or
	// ebay.vercel.app are someone's page, not the shop.
	if _, icann := publicsuffix.PublicSuffix(domain); !icann {
		return false
	}
	label, _, _ := strings.Cut(domain, ".")
	return shopLabels[label]
}

// IsMarketing reports whether the sender's address and counts say bulk
// marketing, per address (order@ and newsletter@ of one domain differ):
//
//   - a campaign-style local part (newsletter@, promotion@, deals@) with bulk
//     evidence: any List-Id or List-Unsubscribe at an unknown domain; at a known
//     shop, carrier or payment domain the address alone (no header needed); or
//   - List-Unsubscribe on most of at least 3 messages of a non-noreply address;
//     at a known shop also with no order-shaped subject (or a List-Id).
//
// Never when most subjects have an order shape, so a shop's order mail that
// also carries List-Unsubscribe stays transactional.
func IsMarketing(in KindInputs, shop bool) bool {
	if in.NMsgs > 0 && in.NTxn*2 > in.NMsgs {
		return false
	}
	lp := localPart(in.Addr)
	unsubMajority := in.NMsgs > 0 && in.NUnsub*2 > in.NMsgs
	if marketingRE.MatchString(lp) {
		if shop {
			// A campaign-style address at a shop says marketing by itself: the
			// shop's order mail does not come from promotion@ or store-news@,
			// and these streams often carry no List-Unsubscribe header.
			return true
		} else if in.NList > 0 || in.NUnsub > 0 {
			return true
		}
	}
	// No-reply addresses keep the notification rule below unless named above.
	if noreplyRE.MatchString(lp) || in.NMsgs < 3 || !unsubMajority {
		return false
	}
	return !shop || in.NTxn == 0 || in.NList > 0
}

// IsTransactionalSubject reports whether a subject has an order, invoice,
// receipt, shipping or payment shape.
// A selling subject counts only when it also carries an order number.
func IsTransactionalSubject(s string) bool {
	if !txnSubjRE.MatchString(s) {
		return false
	}
	return !promoSubjRE.MatchString(s) || orderNumRE.MatchString(s)
}

// ClassifySender applies the rules, in this order, first match wins:
//
//  1. notification: the GitHub notification addresses (notifications@ and
//     noreply@github.com)
//  2. human: the owner has replied to this sender (from the Sent folder, see
//     senders.go), even at a notifier domain
//  3. notification: X-GitHub-Reason, honoured only at github.com (anyone can
//     forge the header)
//  4. notification: a known notifier domain AND a majority of the sender's
//     messages look automated (so a person at cloudflare.com is not caught)
//  5. list: marketing (IsMarketing): a campaign-style local part, or a
//     List-Unsubscribe on most messages, unless most subjects are order mail;
//     even at a shop domain, so promotions are not transactional
//  6. transactional: a known shop, carrier or payment domain
//  7. noreply-style address: transactional when any subject has an order
//     shape, notification when any message carries List-Id or List-Unsubscribe
//  8. list: any message carries a List-Id
//  9. human: everything else
//
// The rules see only headers and subjects, never bodies, so they are
// conservative about "human": a marketing sender without List-Id or
// List-Unsubscribe or a campaign-style address, or a shop on an unlisted
// domain, reads as human.
func ClassifySender(in KindInputs) string {
	domain := in.Domain
	if domain == "" {
		domain = RegistrableDomain(in.Addr)
	}
	isGH := domain == "github.com"
	switch lp := localPart(in.Addr); {
	case isGH && (lp == "notifications" || lp == "noreply"):
		return KindNotification
	case in.NReplied > 0:
		return KindHuman
	case isGH && in.NGH > 0:
		return KindNotification
	case notifierDomains[domain] && in.NMsgs > 0 && in.NAuto*2 > in.NMsgs:
		return KindNotification
	case IsMarketing(in, isShop(domain)):
		return KindList
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
