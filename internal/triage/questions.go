package triage

import (
	"sort"

	"github.com/aliworkshop/ai-engineering-course/internal/jev"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// What we ask the judgment model. This file is to internal/jev what prompt.go is
// to internal/agent: the words, kept apart from the machinery that sends them.
//
// Everything here is exported, and that is not politeness. The benchmark has to
// grade the EXACT questions the agent asks — a benchmark that scores a
// paraphrase of your question tells you about the paraphrase. One definition,
// two callers.
//
// Backticked names in an instruction point at fields of the state. `item` is the
// work item, `kb.billing` the billing article, `draft` the reply we are about to
// send. That is how a question gets to be one sentence about structured data
// instead of a paragraph with the data pasted into it.

// articlePrefix namespaces the per-article questions, so `kb_billing` cannot
// collide with the `category` question in the same request.
const articlePrefix = "kb_"

// ArticleKey is the question id for one knowledge-base topic.
func ArticleKey(topic string) string { return articlePrefix + topic }

// Topics lists the knowledge base's topics in a stable order.
//
// Sorted, because a Go map is not: the order decides the order questions are
// named in an event line and the order articles reach the model, and an agent
// whose briefing is shuffled between runs is one whose replies cannot be
// compared between runs.
func Topics() []string {
	out := make([]string, 0, len(tools.KnowledgeBase))
	for topic := range tools.KnowledgeBase {
		out = append(out, topic)
	}
	sort.Strings(out)
	return out
}

// Category is job 1: which team should handle this item.
//
// A choice over a closed set, so there is no such thing as a fifth category —
// where the model it replaced was handed the same four in a JSON schema and
// asked to fill one in, which is a different thing: that answer was the model's
// own guess about its own guess, and it came back without a confidence.
func Category() jev.Question {
	return jev.Choice(
		"Which support team should handle the customer work item in `item`?",
		map[string]string{
			"billing":   "Charges, payments, invoices, refunds, or subscription billing problems",
			"technical": "Bugs, errors, or features of the product not working as expected",
			"sales":     "Pricing, quotes, buying seats or plans, upgrades, or other purchase questions",
			"other":     "None of the above",
		})
}

// ArticleQuestion is job 2, once per topic: would this article help?
//
// This is retrieval as a judgment rather than as a string match. What it
// replaces asked whether the model's own query text happened to contain the
// word "billing" — so "I was charged twice" retrieved nothing, and "your
// pricing page is broken in Safari" retrieved the price list.
func ArticleQuestion(topic string) jev.Question {
	return jev.Noul(
		"Would the knowledge-base article in `kb."+topic+"` give useful facts "+
			"for replying to the customer work item in `item`?",
		"The article is directly relevant to what the customer needs",
		"The article is about a different problem or request")
}

// TriageQuestions is jobs 1 and 2 in one request: the category, plus one
// question per article.
//
// Asking them together is most of why this is affordable. The model reads the
// state once and evaluates every question against it in parallel, so five
// questions cost barely more than one — which is what makes it reasonable to
// ask about every article in the knowledge base rather than guessing which ones
// are worth asking about.
func TriageQuestions() map[string]jev.Question {
	questions := map[string]jev.Question{"category": Category()}
	for _, topic := range Topics() {
		questions[ArticleKey(topic)] = ArticleQuestion(topic)
	}
	return questions
}

// The two gate questions, job 3. Together they are the whole of what "is this
// reply safe to send?" has been reduced to: does it say only what the articles
// support, and does it answer what the customer actually asked.
const (
	GroundedKey = "grounded"
	OnTopicKey  = "on_topic"
)

// VerifyQuestions is job 3: the check that runs before the irreversible send.
//
// The grounded question carves out courtesy on purpose. "Sorry about that" and
// "a specialist will follow up" are not factual claims, and a check that
// treated them as claims would block every polite reply ever written — which is
// the failure mode that gets a gate switched off.
func VerifyQuestions() map[string]jev.Question {
	return map[string]jev.Question{
		GroundedKey: jev.Noul(
			"Is every factual claim in `draft` (prices, timelines, causes, workarounds, "+
				"ticket numbers, policies) stated in or directly implied by `articles`? "+
				"Apologies, courtesy, and offers to help or follow up are not factual claims.",
			"All factual claims in the draft are supported by the articles",
			"The draft states at least one fact, number, or promise the articles do not support"),
		OnTopicKey: jev.Noul(
			"Does `draft` respond to the specific problem or request in `customer_message`?",
			"The draft addresses what the customer asked about",
			"The draft ignores or misreads what the customer asked about"),
	}
}
