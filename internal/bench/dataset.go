package bench

// The hand-labelled cases, ported from the workshop's dataset unchanged.
//
// Fifty-four of them, and the labels are the whole asset: a benchmark is only
// ever as good as someone's willingness to sit down and decide what the right
// answer was. Read the tags and you can see what each case is probing —
// paraphrase, negation, non-English, slang, and the keyword traps that the
// substring matcher this replaces got wrong every single time.
//
// Two label vocabularies in the triage set, and the distinction matters. Need
// is an article a good reply cannot do without; OK is one that would be
// defensible to include and pointless to require. An article in neither is a
// false positive. Without that middle category every borderline case becomes an
// argument, and a benchmark you argue with is one you stop running.

// TriageCase grades jobs 1 and 2: the category, and which articles a reply
// needs.
type TriageCase struct {
	ID       string
	Text     string
	Category string
	// AlsoOK lists categories that would also be defensible. Some items
	// genuinely belong to two teams, and a benchmark that pretends otherwise
	// measures the labeller's coin flip.
	AlsoOK []string
	Need   []string
	OK     []string
	Tag    string
}

// VerifyCase grades job 3: a draft, the articles it was written from, and
// whether it is grounded and on topic.
type VerifyCase struct {
	ID       string
	Customer string
	Topics   []string
	Draft    string
	Grounded bool
	OnTopic  bool
	Tag      string
}

// Triage is the classify-and-retrieve set.
var Triage = []TriageCase{
	// billing
	{ID: "t01", Text: "I was charged twice this month.", Category: "billing", Need: []string{"billing"}, OK: []string{"refund"}, Tag: "plain"},
	{ID: "t02", Text: "My card got billed $40 but my plan is $20, what happened?", Category: "billing", Need: []string{"billing"}, OK: []string{"refund"}, Tag: "paraphrase"},
	{ID: "t03", Text: "Where can I download last month's invoice?", Category: "billing", OK: []string{"billing"}, Tag: "no article fits"},
	{ID: "t04", Text: "I want a refund for the annual plan I bought yesterday.", Category: "billing", Need: []string{"refund"}, OK: []string{"billing"}, Tag: "plain"},
	{ID: "t05", Text: "When will my refund show up? It's been a week.", Category: "billing", Need: []string{"refund"}, Tag: "plain"},
	{ID: "t06", Text: "Two identical pending charges on my statement from you guys", Category: "billing", Need: []string{"billing"}, OK: []string{"refund"}, Tag: "paraphrase"},
	{ID: "t07", Text: "Ich wurde diesen Monat zweimal belastet, bitte helfen Sie mir.", Category: "billing", Need: []string{"billing"}, OK: []string{"refund"}, Tag: "non-English"},
	{ID: "t08", Text: "Cancel my subscription and stop charging me", Category: "billing", OK: []string{"billing", "refund"}, Tag: "no article fits"},
	{ID: "t09", Text: "lol got billed 2x again 🙃", Category: "billing", Need: []string{"billing"}, OK: []string{"refund"}, Tag: "slang"},

	// technical
	{ID: "t10", Text: "The export button fails on Safari.", Category: "technical", Need: []string{"export"}, Tag: "plain"},
	{ID: "t11", Text: "Exporting a report on my Mac in Safari just spins forever", Category: "technical", Need: []string{"export"}, Tag: "paraphrase"},
	{ID: "t12", Text: "The 'Download data' button does nothing when I use Safari", Category: "technical", Need: []string{"export"}, Tag: "no keyword"},
	{ID: "t13", Text: "I can't log in, it says invalid token.", Category: "technical", Tag: "no article fits"},
	{ID: "t14", Text: "The dashboard charts are blank since this morning's update", Category: "technical", Tag: "no article fits"},
	{ID: "t15", Text: "App crashes when I upload a 2GB file", Category: "technical", Tag: "no article fits"},
	{ID: "t16", Text: "Your pricing page won't load in Safari, it's just a white screen", Category: "technical", OK: []string{"pricing"}, Tag: "keyword trap"},
	{ID: "t17", Text: "Refunds are fine, I don't need one. I just need export to work in Safari.", Category: "technical", AlsoOK: []string{"billing"}, Need: []string{"export"}, Tag: "negation"},

	// sales
	{ID: "t18", Text: "Can you send pricing for 50 seats?", Category: "sales", Need: []string{"pricing"}, Tag: "plain"},
	{ID: "t19", Text: "How much would it cost for a team of 30?", Category: "sales", Need: []string{"pricing"}, Tag: "paraphrase"},
	{ID: "t20", Text: "Do you offer discounts for larger teams?", Category: "sales", Need: []string{"pricing"}, Tag: "paraphrase"},
	{ID: "t21", Text: "We'd like to go from 10 to 40 seats. What would that run us?", Category: "sales", AlsoOK: []string{"billing"}, Need: []string{"pricing"}, Tag: "paraphrase"},
	{ID: "t22", Text: "Can we get a formal quote for our procurement team?", Category: "sales", OK: []string{"pricing"}, Tag: "plain"},
	{ID: "t23", Text: "Is there an enterprise plan with SSO?", Category: "sales", OK: []string{"pricing"}, Tag: "no article fits"},
	{ID: "t24", Text: "Not a bug report, just curious: what does one seat cost?", Category: "sales", Need: []string{"pricing"}, Tag: "negation"},
	{ID: "t25", Text: "¿Cuánto cuesta el plan de equipo por usuario?", Category: "sales", Need: []string{"pricing"}, Tag: "non-English"},

	// other
	{ID: "t26", Text: "Just wanted to say your product is great!", Category: "other", Tag: "plain"},
	{ID: "t27", Text: "Are you hiring frontend engineers?", Category: "other", Tag: "plain"},
	{ID: "t28", Text: "What's your office address for sending a letter?", Category: "other", Tag: "plain"},
	{ID: "t29", Text: "Can I come on your podcast to talk about data exports?", Category: "other", Tag: "keyword trap"},
	{ID: "t30", Text: "My friend says you charge too much. Is that true?", Category: "other", AlsoOK: []string{"sales"}, OK: []string{"pricing"}, Tag: "keyword trap"},
}

// goodBilling is the reply the billing articles actually support, reused where a
// case is about something other than the draft.
const goodBilling = "Sorry about the double charge! Double charges are usually a duplicate " +
	"authorization that drops off in 3-5 days. If it has already settled, we'll refund it " +
	"immediately, and refunds post in 5-10 business days."

// Verify is the gate set: twelve drafts that should go out and twelve that
// should not.
var Verify = []VerifyCase{
	// billing
	{ID: "v01", Customer: "I was charged twice and need help.", Topics: []string{"billing", "refund"},
		Draft: goodBilling, Grounded: true, OnTopic: true, Tag: "good"},
	{ID: "v02", Customer: "I was charged twice and need help.", Topics: []string{"billing", "refund"},
		Draft:    "Sorry about that! The duplicate charge will drop off within 24 hours.",
		Grounded: false, OnTopic: true, Tag: "wrong timeline"},
	{ID: "v03", Customer: "I was charged twice and need help.", Topics: []string{"billing", "refund"},
		Draft:    "Apologies! We've added a $50 credit to your account for the trouble.",
		Grounded: false, OnTopic: true, Tag: "invented promise"},
	{ID: "v04", Customer: "I was charged twice and need help.", Topics: []string{"billing", "refund"},
		Draft:    "Sorry about that! A billing specialist will look into the double charge and follow up shortly.",
		Grounded: true, OnTopic: true, Tag: "no facts, courteous"},
	{ID: "v05", Customer: "I was charged twice and need help.", Topics: []string{"billing", "refund"},
		Draft:    "The Safari export failure is a known bug. Please use Chrome instead.",
		Grounded: false, OnTopic: false, Tag: "wrong item"},
	{ID: "v06", Customer: "I was charged twice and need help.", Topics: []string{"billing", "refund"},
		Draft:    "The duplicate charge is a known bug (TICKET-4412) that engineering is fixing.",
		Grounded: false, OnTopic: true, Tag: "conflated facts"},
	{ID: "v07", Customer: "I was charged twice and need help.", Topics: []string{"billing", "refund"},
		Draft:    "Double charges always drop off in 3-5 days, so a refund is never needed.",
		Grounded: false, OnTopic: true, Tag: "contradicts KB"},
	{ID: "v08", Customer: "Ich wurde zweimal belastet.", Topics: []string{"billing", "refund"},
		Draft: goodBilling, Grounded: true, OnTopic: true, Tag: "cross-language"},

	// refund
	{ID: "v09", Customer: "When will my refund show up?", Topics: []string{"refund"},
		Draft:    "Refunds post in 5-10 business days. If you're on a Pro account, we can expedite it.",
		Grounded: true, OnTopic: true, Tag: "good"},
	{ID: "v10", Customer: "When will my refund show up?", Topics: []string{"refund"},
		Draft:    "Your refund will arrive tomorrow morning.",
		Grounded: false, OnTopic: true, Tag: "wrong timeline"},

	// export
	{ID: "v11", Customer: "The export button fails on Safari.", Topics: []string{"export"},
		Draft:    "Thanks for reporting this! It's a known bug (TICKET-4412). As a workaround, please use Chrome or the CSV export.",
		Grounded: true, OnTopic: true, Tag: "good"},
	{ID: "v12", Customer: "The export button fails on Safari.", Topics: []string{"export"},
		Draft:    "This is tracked as TICKET-9981 and will be fixed in next week's release.",
		Grounded: false, OnTopic: true, Tag: "invented ticket + date"},
	{ID: "v13", Customer: "The export button fails on Safari.", Topics: []string{"export"},
		Draft:    "That's a known issue. Switching to Firefox should fix it.",
		Grounded: false, OnTopic: true, Tag: "wrong workaround"},
	{ID: "v14", Customer: "The export button fails on Safari.", Topics: []string{"export"},
		Draft:    "We're really sorry for the inconvenience and appreciate your patience. It's a known bug (TICKET-4412); Chrome or the CSV export will work in the meantime.",
		Grounded: true, OnTopic: true, Tag: "good, padded"},
	{ID: "v15", Customer: "The export button fails on Safari.", Topics: []string{"export", "refund"},
		Draft:    "Refunds post in 5-10 business days, and Pro accounts can be expedited.",
		Grounded: true, OnTopic: false, Tag: "grounded but off-topic"},
	{ID: "v16", Customer: "The export button fails on Safari.", Topics: []string{"export"},
		Draft:    "Team plans are $20/seat/mo with a volume discount at 25+ seats.",
		Grounded: false, OnTopic: false, Tag: "wrong item"},

	// pricing
	{ID: "v17", Customer: "Can you send pricing for 50 seats?", Topics: []string{"pricing"},
		Draft:    "Team plans are $20 per seat per month, and at 50 seats you'd qualify for our volume discount (25+ seats).",
		Grounded: true, OnTopic: true, Tag: "good"},
	{ID: "v18", Customer: "Can you send pricing for 50 seats?", Topics: []string{"pricing"},
		Draft:    "Team plans are $15 per seat per month.",
		Grounded: false, OnTopic: true, Tag: "wrong number"},
	{ID: "v19", Customer: "Can you send pricing for 50 seats?", Topics: []string{"pricing"},
		Draft:    "Great news: 50 seats gets you 40% off, so it's $12/seat/mo.",
		Grounded: false, OnTopic: true, Tag: "invented discount"},
	{ID: "v20", Customer: "Can you send pricing for 50 seats?", Topics: []string{"pricing"},
		Draft:    "At $20/seat/mo, 50 seats comes to $1,000/mo before the volume discount that applies at 25+ seats.",
		Grounded: true, OnTopic: true, Tag: "derived arithmetic"},
	{ID: "v21", Customer: "Do you offer a free trial?", Topics: []string{"pricing"},
		Draft:    "Team plans are $20/seat/mo with a volume discount at 25+ seats.",
		Grounded: true, OnTopic: false, Tag: "grounded but off-topic"},

	// no articles at all
	{ID: "v22", Customer: "Are you hiring frontend engineers?",
		Draft:    "Thanks for asking! Someone from our team will follow up with you.",
		Grounded: true, OnTopic: true, Tag: "no facts, courteous"},
	{ID: "v23", Customer: "Are you hiring frontend engineers?",
		Draft:    "Yes! We have 5 open frontend roles paying $150k, apply by Friday.",
		Grounded: false, OnTopic: true, Tag: "hallucinated facts"},
	{ID: "v24", Customer: "I can't log in, it says invalid token.",
		Draft:    "Thanks for your interest in our team plans! A sales rep will reach out.",
		Grounded: true, OnTopic: false, Tag: "misread request"},
}
