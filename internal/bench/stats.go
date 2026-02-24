package bench

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
)

// WelchTTest performs Welch's two-sample t-test (unequal variances).
func WelchTTest(a, b []float64, alpha float64) (tStat, pValue float64, significant bool) {
	na, nb := float64(len(a)), float64(len(b))
	if na < 2 || nb < 2 {
		return 0, 1, false
	}

	ma, mb := mean(a), mean(b)
	va, vb := variance(a), variance(b)

	se := math.Sqrt(va/na + vb/nb)
	if se == 0 {
		return 0, 1, false
	}

	tStat = (ma - mb) / se

	// Welch-Satterthwaite degrees of freedom.
	num := math.Pow(va/na+vb/nb, 2)
	denom := math.Pow(va/na, 2)/(na-1) + math.Pow(vb/nb, 2)/(nb-1)
	if denom == 0 {
		return tStat, 1, false
	}
	df := num / denom

	// Approximate two-tailed p-value using regularized incomplete beta function.
	pValue = tDistPValue(math.Abs(tStat), df)
	significant = pValue < alpha
	return
}

// BootstrapCI computes a bootstrap confidence interval for the percentage difference between means.
func BootstrapCI(baseline, treatment []float64, nBoot int, alpha float64) (lower, upper float64) {
	if len(baseline) == 0 || len(treatment) == 0 {
		return 0, 0
	}
	if nBoot <= 0 {
		nBoot = 10000
	}

	rng := rand.New(rand.NewSource(42))
	diffs := make([]float64, nBoot)

	for i := 0; i < nBoot; i++ {
		bMean := bootstrapMean(baseline, rng)
		tMean := bootstrapMean(treatment, rng)
		if bMean == 0 {
			diffs[i] = 0
		} else {
			diffs[i] = (tMean - bMean) / bMean * 100
		}
	}

	sort.Float64s(diffs)
	loIdx := int(math.Floor(alpha / 2 * float64(nBoot)))
	hiIdx := int(math.Floor((1 - alpha/2) * float64(nBoot)))
	if loIdx < 0 {
		loIdx = 0
	}
	if hiIdx >= nBoot {
		hiIdx = nBoot - 1
	}
	return diffs[loIdx], diffs[hiIdx]
}

// CohenD computes Cohen's d effect size.
func CohenD(a, b []float64) float64 {
	na, nb := float64(len(a)), float64(len(b))
	if na < 2 || nb < 2 {
		return 0
	}

	ma, mb := mean(a), mean(b)
	va, vb := variance(a), variance(b)

	// Pooled standard deviation.
	pooledVar := ((na-1)*va + (nb-1)*vb) / (na + nb - 2)
	if pooledVar == 0 {
		return 0
	}
	return (ma - mb) / math.Sqrt(pooledVar)
}

// ComparisonSummary generates a Markdown comparison table with significance markers.
func ComparisonSummary(baseline, treatment []BenchResult, baselineName, treatmentName string) string {
	if len(baseline) == 0 || len(treatment) == 0 {
		return "Insufficient data for comparison.\n"
	}

	bCosts := extractCosts(baseline)
	tCosts := extractCosts(treatment)
	bDurations := extractDurations(baseline)
	tDurations := extractDurations(treatment)
	bAssertRates := extractAssertRates(baseline)
	tAssertRates := extractAssertRates(treatment)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("| Metric | %s | %s | Delta | p-value | Sig |\n", baselineName, treatmentName))
	sb.WriteString("|--------|")
	sb.WriteString(strings.Repeat("-", len(baselineName)+2))
	sb.WriteString("|")
	sb.WriteString(strings.Repeat("-", len(treatmentName)+2))
	sb.WriteString("|-------|---------|-----|\n")

	writeComparisonRow(&sb, "Cost (USD)", bCosts, tCosts, "$%.2f", true)
	writeComparisonRow(&sb, "Duration (s)", bDurations, tDurations, "%.1f", false)
	writeComparisonRow(&sb, "Assert Rate", bAssertRates, tAssertRates, "%.0f%%", false)

	return sb.String()
}

func writeComparisonRow(sb *strings.Builder, label string, a, b []float64, format string, showDollar bool) {
	ma, sa := mean(a), stddev(a)
	mb, sb2 := mean(b), stddev(b)

	delta := ""
	if ma != 0 {
		pctDiff := (mb - ma) / ma * 100
		delta = fmt.Sprintf("%+.0f%%", pctDiff)
	}

	_, pValue, _ := WelchTTest(a, b, 0.05)
	sig := significanceMarker(pValue)

	fmtVal := func(m, s float64) string {
		return fmt.Sprintf(format+" ± "+format, m, s)
	}

	sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %.3f | %s |\n",
		label, fmtVal(ma, sa), fmtVal(mb, sb2), delta, pValue, sig))
}

func significanceMarker(p float64) string {
	switch {
	case p < 0.001:
		return "***"
	case p < 0.01:
		return "**"
	case p < 0.05:
		return "*"
	default:
		return ""
	}
}

func extractCosts(results []BenchResult) []float64 {
	vals := make([]float64, len(results))
	for i, r := range results {
		vals[i] = r.EstCostUSD
	}
	return vals
}

func extractDurations(results []BenchResult) []float64 {
	vals := make([]float64, len(results))
	for i, r := range results {
		vals[i] = r.Duration.Seconds()
	}
	return vals
}

func extractAssertRates(results []BenchResult) []float64 {
	vals := make([]float64, len(results))
	for i, r := range results {
		total := r.AssertsPassed + r.AssertsFailed
		if total > 0 {
			vals[i] = float64(r.AssertsPassed) / float64(total) * 100
		}
	}
	return vals
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func stddev(vals []float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	m := mean(vals)
	sumSq := 0.0
	for _, v := range vals {
		d := v - m
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(len(vals)-1))
}

func variance(vals []float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	m := mean(vals)
	sumSq := 0.0
	for _, v := range vals {
		d := v - m
		sumSq += d * d
	}
	return sumSq / float64(len(vals)-1)
}

func bootstrapMean(vals []float64, rng *rand.Rand) float64 {
	n := len(vals)
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += vals[rng.Intn(n)]
	}
	return sum / float64(n)
}

// tDistPValue approximates the two-tailed p-value for Student's t-distribution.
func tDistPValue(t, df float64) float64 {
	if df <= 0 {
		return 1
	}
	x := df / (df + t*t)
	// P = I_x(df/2, 1/2) where I_x is the regularized incomplete beta function.
	p := regIncBeta(x, df/2, 0.5)
	return p
}

// regIncBeta computes I_x(a, b) via Lentz's continued fraction.
func regIncBeta(x, a, b float64) float64 {
	if x < 0 || x > 1 {
		return 0
	}
	if x == 0 {
		return 0
	}
	if x == 1 {
		return 1
	}

	// Use the symmetry relation for better convergence.
	if x > (a+1)/(a+b+2) {
		return 1 - regIncBeta(1-x, b, a)
	}

	lnBeta := lgamma(a) + lgamma(b) - lgamma(a+b)
	front := math.Exp(math.Log(x)*a+math.Log(1-x)*b-lnBeta) / a

	// Lentz's continued fraction.
	f := 1.0
	c := 1.0
	d := 1.0 - (a+b)*x/(a+1)
	if math.Abs(d) < 1e-30 {
		d = 1e-30
	}
	d = 1 / d
	f = d

	for i := 1; i <= 200; i++ {
		m := float64(i)
		// Even step.
		num := m * (b - m) * x / ((a + 2*m - 1) * (a + 2*m))
		d = 1 + num*d
		if math.Abs(d) < 1e-30 {
			d = 1e-30
		}
		c = 1 + num/c
		if math.Abs(c) < 1e-30 {
			c = 1e-30
		}
		d = 1 / d
		f *= d * c

		// Odd step.
		num = -(a + m) * (a + b + m) * x / ((a + 2*m) * (a + 2*m + 1))
		d = 1 + num*d
		if math.Abs(d) < 1e-30 {
			d = 1e-30
		}
		c = 1 + num/c
		if math.Abs(c) < 1e-30 {
			c = 1e-30
		}
		d = 1 / d
		delta := d * c
		f *= delta

		if math.Abs(delta-1) < 1e-10 {
			break
		}
	}

	return front * f
}

func lgamma(x float64) float64 {
	v, _ := math.Lgamma(x)
	return v
}
