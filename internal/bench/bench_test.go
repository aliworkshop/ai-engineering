package bench

import (
	"math"
	"testing"

	"github.com/aliworkshop/ai-engineering-course/internal/triage"
)

func nouls(billing, refund, export, pricing float64) map[string]float64 {
	return map[string]float64{
		triage.ArticleKey("billing"): billing,
		triage.ArticleKey("refund"):  refund,
		triage.ArticleKey("export"):  export,
		triage.ArticleKey("pricing"): pricing,
	}
}

// The three-way label is the whole reason this scoring is not a one-liner: an
// article that is merely ALLOWED is neither a hit nor a miss, and counting it
// either way would make half the dataset argue with itself.
func TestScoreTriageTreatsOptionalArticlesAsNeitherRightNorWrong(t *testing.T) {
	c := TriageCase{
		ID: "t01", Category: "billing",
		Need: []string{"billing"}, OK: []string{"refund"},
	}
	// Picked the needed one, the optional one, and one that is neither.
	got := scoreTriage(c, Call{Category: "billing", Nouls: nouls(0.9, 0.8, 0.7, 0.1)}, 0.5)

	if got.TP != 1 {
		t.Errorf("tp = %d, want 1 — the needed article", got.TP)
	}
	if got.FP != 1 {
		t.Errorf("fp = %d, want 1 — export is outside need ∪ ok", got.FP)
	}
	if got.FN != 0 {
		t.Errorf("fn = %d, want 0", got.FN)
	}
	if got.ArticlesOK {
		t.Error("a false positive was scored as an exact match")
	}
	if !got.CategoryOK {
		t.Error("the right category was not counted")
	}
}

func TestScoreTriageAcceptsADefensibleCategory(t *testing.T) {
	c := TriageCase{ID: "t17", Category: "technical", AlsoOK: []string{"billing"}}
	if !scoreTriage(c, Call{Category: "billing", Nouls: nouls(0, 0, 0, 0)}, 0.5).CategoryOK {
		t.Error("a category listed as also acceptable was counted wrong")
	}
	if scoreTriage(c, Call{Category: "sales", Nouls: nouls(0, 0, 0, 0)}, 0.5).CategoryOK {
		t.Error("a category that is not acceptable was counted right")
	}
}

func TestScoreTriageCountsAMissedArticle(t *testing.T) {
	c := TriageCase{ID: "t05", Category: "billing", Need: []string{"refund"}}
	got := scoreTriage(c, Call{Category: "billing", Nouls: nouls(0, 0.4, 0, 0)}, 0.5)
	if got.FN != 1 || got.TP != 0 || got.ArticlesOK {
		t.Errorf("score = %+v, want one miss and no exact match", got)
	}
}

// The verdict is what emails a customer, so it is scored separately from the
// two probabilities that produced it: both can be off and still land right.
func TestScoreVerifyGradesTheDecision(t *testing.T) {
	bad := VerifyCase{ID: "v19", Grounded: false, OnTopic: true}
	got := scoreVerify(bad, Call{Nouls: map[string]float64{
		triage.GroundedKey: 0.02, triage.OnTopicKey: 0.95,
	}}, 0.5)
	if !got.Blocked || !got.ShouldBlock || !got.DecisionOK {
		t.Errorf("score = %+v, want a bad draft caught", got)
	}

	good := VerifyCase{ID: "v01", Grounded: true, OnTopic: true}
	got = scoreVerify(good, Call{Nouls: map[string]float64{
		triage.GroundedKey: 0.9, triage.OnTopicKey: 0.9,
	}}, 0.5)
	if got.Blocked || got.ShouldBlock || !got.DecisionOK {
		t.Errorf("score = %+v, want a good draft passed", got)
	}
}

// Brier is the number that says whether a threshold means anything: a system
// that is always right but always says 0.51 has good accuracy and a bad Brier.
func TestBrierRewardsCalibration(t *testing.T) {
	// One case, one needed article, three that are neither needed nor allowed.
	triageCases := []TriageCase{{ID: "x", Category: "billing", Need: []string{"billing"}}}
	verifyCases := []VerifyCase{{ID: "y", Grounded: true, OnTopic: true}}

	saved := Triage
	savedVerify := Verify
	Triage, Verify = triageCases, verifyCases
	defer func() { Triage, Verify = saved, savedVerify }()

	perfect := brier(
		[]Call{{Nouls: nouls(1, 0, 0, 0)}},
		[]Call{{Nouls: map[string]float64{triage.GroundedKey: 1, triage.OnTopicKey: 1}}})
	if perfect.Articles != 0 || perfect.Gate != 0 {
		t.Errorf("brier = %+v, want 0 for a perfectly calibrated answer", perfect)
	}

	coinFlip := brier(
		[]Call{{Nouls: nouls(0.5, 0.5, 0.5, 0.5)}},
		[]Call{{Nouls: map[string]float64{triage.GroundedKey: 0.5, triage.OnTopicKey: 0.5}}})
	if math.Abs(coinFlip.Articles-0.25) > 1e-9 || math.Abs(coinFlip.Gate-0.25) > 1e-9 {
		t.Errorf("brier = %+v, want 0.25 for a coin flip", coinFlip)
	}
}

func TestPercentile(t *testing.T) {
	xs := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := percentile(xs, 0.5); got != 60 {
		t.Errorf("p50 = %v, want 60", got)
	}
	if got := percentile(xs, 1); got != 100 {
		t.Errorf("max = %v, want 100", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("p50 of nothing = %v, want 0", got)
	}
}

// Every case has to name a topic the knowledge base actually has, or the
// benchmark grades an article nobody was shown.
func TestDatasetTopicsExist(t *testing.T) {
	known := map[string]bool{}
	for _, topic := range triage.Topics() {
		known[topic] = true
	}
	for _, c := range Triage {
		for _, topic := range append(append([]string{}, c.Need...), c.OK...) {
			if !known[topic] {
				t.Errorf("%s names an unknown article %q", c.ID, topic)
			}
		}
	}
	for _, c := range Verify {
		for _, topic := range c.Topics {
			if !known[topic] {
				t.Errorf("%s names an unknown article %q", c.ID, topic)
			}
		}
	}
}
