package indexcov

import (
	"bufio"
	"fmt"
	"html/template"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	arg "github.com/alexflint/go-arg"
	"github.com/biogo/hts/bam"
	"github.com/biogo/hts/bgzf"
	"github.com/biogo/hts/sam"
	chartjs "github.com/brentp/go-chartjs"
	"github.com/brentp/go-chartjs/types"
	"github.com/gonum/floats"
	"github.com/gonum/matrix/mat64"
	"github.com/gonum/stat"
)

// Ploidy indicates the expected ploidy of the samples.
var Ploidy = 2

var cli = &struct {
	Prefix    string   `arg:"-p,required,help:prefix for output files"`
	IncludeGL bool     `arg:"-e,help:plot GL chromosomes like: GL000201.1 which are not plotted by default"`
	Sex       []string `arg:"-X,help:name of the sex chromosome(s) used to infer sex; The first will be used to populate the sex column in a ped file."`
	Chrom     string   `arg:"-c,help:optional chromosome to extract depth. default is entire genome."`
	Bam       []string `arg:"positional,required,help:bam(s) for which to estimate coverage"`
}{Sex: []string{"X", "Y"}}

// MaxCN is the maximum normalized value.
var MaxCN = float32(6)

// Index wraps a bam.Index to cache calculated values.
type Index struct {
	*bam.Index

	//mu                *sync.RWMutex
	medianSizePerTile float64
	refs              [][]int64
}

func vOffset(o bgzf.Offset) int64 {
	return o.File<<16 | int64(o.Block)
}

// init sets the medianSizePerTile
func (x *Index) init() {
	x.refs = getRefs(x.Index)
	x.Index = nil

	// sizes is used to get the median.
	sizes := make([]int64, 0, 16384)
	for k := 0; k < len(x.refs)-1; k++ {
		if len(x.refs[k]) < 2 {
			continue
		}
		for i, iv := range x.refs[k][1:] {
			sizes = append(sizes, iv-x.refs[k][i])
		}
	}

	// we get the median as it's more stable than mean.
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })
	x.medianSizePerTile = float64(sizes[len(sizes)/2])
	// if we have a single chunk of a chrom, then we get a lot of zeros so we address that here.
	if x.medianSizePerTile == 0 {
		i := len(sizes) / 2
		for ; i < len(sizes) && sizes[i] == 0; i++ {
		}
		sizes = sizes[i:]
		x.medianSizePerTile = float64(sizes[len(sizes)/2])
	}
}

// NormalizedDepth returns a list of numbers for the normalized depth of the given region.
// Values are scaled to have a mean of 1. If end is 0, the full chromosome is returned.
func (x *Index) NormalizedDepth(refID int, start int, end int) []float32 {

	if x.medianSizePerTile == 0.0 {
		x.init()
	}
	ref := x.refs[refID]

	si, ei := start/TileWidth, end/TileWidth
	if end == 0 || ei >= len(ref) {
		ei = len(ref) - 1
	}
	if ei <= si {
		return nil
	}
	depths := make([]float32, 0, ei-si)
	for i, o := range ref[si:ei] {
		depths = append(depths, float32(float64(ref[si+i+1]-o)/x.medianSizePerTile))
		if depths[i] > MaxCN {
			depths[i] = MaxCN
		}
	}
	return depths
}

const slots = 70

// with 0.5, we'll get centered at 1 and max of 2.
// so the max is 1/slotsMid
const slotsMid = float64(2) / float64(3)

func tint(f float32) int {
	if v := int(f); v < slots {
		return v
	}
	return slots - 1
}

// CountsAtDepth calculates the count of items in depths that are at 100 * d
func CountsAtDepth(depths []float32, counts []int) {
	if len(counts) != slots {
		panic(fmt.Sprintf("indexcov: expecting counts to be length %d", slots))
	}
	for _, d := range depths {
		counts[tint(d*(slots*float32(slotsMid))+0.5)]++
	}
}

// CountsROC returns a slice that indicates the cumulative proportion of
// 16KB chunks that were at least (normalized) depth given by their index.
func CountsROC(counts []int) []float32 {
	totals := make([]int, len(counts))
	totals[len(totals)-1] = counts[len(totals)-1]
	for i := len(totals) - 2; i >= 0; i-- {
		totals[i] = totals[i+1] + counts[i]
	}
	max := float32(totals[0])
	roc := make([]float32, len(counts))
	for i := 0; i < len(roc); i++ {
		roc[i] = float32(totals[i]) / max
	}
	return roc
}

func getRef(b *bam.Reader, chrom string) *sam.Reference {
	refs := b.Header().Refs()
	if strings.HasPrefix(chrom, "chr") {
		chrom = chrom[3:]
	}
	for _, ref := range refs {
		if chrom == ref.Name() {
			return ref
		}
		if strings.HasPrefix(ref.Name(), "chr") {
			if chrom == ref.Name()[3:] {
				return ref
			}
		}
	}
	return nil
}

func getShortName(b string) string {

	fh, err := os.Open(b)
	if err != nil {
		log.Fatal(err)
	}
	defer fh.Close()
	br, err := bam.NewReader(fh, 1)
	if err != nil {
		log.Fatal(err)
	}
	defer br.Close()
	m := make(map[string]bool)
	for _, rg := range br.Header().RGs() {
		m[rg.Get(sam.Tag([2]byte{'S', 'M'}))] = true
	}
	if len(m) > 1 {
		log.Printf("warning: more than one tag for %s", b)
	}
	for sm := range m {
		return sm
	}
	vs := strings.Split(b, "/")
	v := vs[len(vs)-1]
	vs = strings.SplitN(v, ".", 1)
	return vs[len(vs)-1]
}

func getWriter(prefix string) (*bgzf.Writer, error) {
	fh, err := os.Create(fmt.Sprintf("%s-indexcov.bed.gz", prefix))
	if err != nil {
		return nil, err
	}
	w := bgzf.NewWriter(fh, 1)
	w.ModTime = time.Unix(0, 0)
	w.OS = 0xff
	return w, nil
}

func zero(ints []int) {
	for i := range ints {
		ints[i] = 0
	}
}

// Main is called from the goleft dispatcher
func Main() {

	chartjs.XFloatFormat = "%.0f"
	p := arg.MustParse(cli)
	if len(cli.Bam) == 0 {
		p.Fail("indexcov: expected at least 1 bam")
	}
	if strings.HasSuffix(cli.Prefix, "/") {
		cli.Prefix = cli.Prefix + "qc"
	}

	rdr, err := os.Open(cli.Bam[0])
	if err != nil {
		log.Println(cli.Bam[0])
		panic(err)
	}
	brdr, err := bam.NewReader(rdr, 2)
	if err != nil {
		panic(err)
	}

	var refs []*sam.Reference

	if cli.Chrom != "" {
		refs = append(refs, getRef(brdr, cli.Chrom))
	} else {
		refs = brdr.Header().Refs()
	}
	rdr.Close()
	brdr.Close()
	if refs == nil {
		panic(fmt.Sprintf("indexcov: chromosome: %s not found", cli.Chrom))
	}

	var idxs []*Index
	names := make([]string, 0, len(cli.Bam))

	for _, b := range cli.Bam {

		rdr, err = os.Open(b + ".bai")
		if err != nil {
			var terr error
			rdr, terr = os.Open(b[:(len(b)-4)] + ".bai")
			if terr != nil {
				panic(err)
			}
		}

		idx, err := bam.ReadIndex(bufio.NewReader(rdr))
		if err != nil {
			panic(err)
		}
		idxs = append(idxs, &Index{Index: idx})
		names = append(names, getShortName(b))
	}

	charts, sexes, counts, pca8, chromNames := run(refs, idxs, names)

	chartjs.XFloatFormat = "%.2f"
	saveCharts(fmt.Sprintf("%s-indexcov-roc.html", cli.Prefix), "", charts...)
	writeIndex(sexes, counts, cli.Sex, names, cli.Prefix, pca8, chromNames)
}

// if there are more samples than this then the depth plots won't be drawn.
const maxSamples = 100

func run(refs []*sam.Reference, idxs []*Index, names []string) ([]chartjs.Chart, map[string][]float64, []*counter, [][]uint8, []string) {
	// keep a slice of charts since we plot all of the coverage roc charts in a single html file.
	charts := make([]chartjs.Chart, 0, len(refs))
	sexes := make(map[string][]float64)
	counts := make([][]int, len(idxs))
	depths := make([][]float32, len(idxs))

	offs := make([]*counter, len(idxs))
	// uint8 to use less memory.
	pca8 := make([][]uint8, len(idxs))
	log.Printf("indexcov: running on %d indexes", len(idxs))
	if len(idxs) > maxSamples {
		log.Printf("indexcov: only plotting ROC, sex, PCA and bin plots (not depth) because # of samples %d is > %d\n", len(idxs), maxSamples)
	}

	tmp, err := getWriter(cli.Prefix)
	if err != nil {
		panic(err)
	}
	defer tmp.Close()
	bgz := bufio.NewWriter(tmp)
	defer bgz.Flush()

	rtmp, err := os.Create(fmt.Sprintf("%s-indexcov.roc", cli.Prefix))
	if err != nil {
		panic(err)
	}
	defer rtmp.Close()
	rfh := bufio.NewWriter(rtmp)
	defer rfh.Flush()
	chromNames := make([]string, 0, len(refs))

	fmt.Fprintf(bgz, "#chrom\tstart\tend\t%s\n", strings.Join(names, "\t"))
	for ir, ref := range refs {
		chrom := ref.Name()
		// Some samples may not have all the data, so we always take the longest sample for printing.
		longest, longesti := 0, 0

		for k, idx := range idxs {
			if ir == 0 {
				pca8[k] = make([]uint8, 0, 2e5)
				offs[k] = &counter{}
			}
			depths[k] = idx.NormalizedDepth(ref.ID(), 0, ref.Len())
			if len(depths[k]) > longest {
				longesti = k
				longest = len(depths[k])
			}
			if ir == 0 {
				counts[k] = make([]int, slots)
			} else {
				zero(counts[k])
			}

			CountsAtDepth(depths[k], counts[k])
		}

		isSex := false
		for _, x := range cli.Sex {
			if x == chrom {
				isSex = true
				if len(depths[longesti]) > 0 {
					sexes[chrom] = GetCN(depths)
				}
			}
		}
		if !isSex {
			// now add non-sex chromosomes to the pca data since we know the longest.
			for k := range idxs {
				var i int
				for i = 0; i < len(depths[k]); i++ {
					pca8[k] = append(pca8[k], uint8(65535/MaxCN*depths[k][i]+0.5))
				}
				for ; i < longest; i++ {
					pca8[k] = append(pca8[k], 0)
				}
				offs[k].count(depths[k], longest)
			}
		}

		for i := 0; i < len(depths[longesti]); i++ {
			fmt.Fprintf(bgz, "%s\t%d\t%d\t%s\n", chrom, i*16384, (i+1)*16384, depthsFor(depths, i))
		}
		if len(depths[longesti]) > 0 {
			c := writeROCs(counts, names, chrom, cli.Prefix, rfh)
			if cli.IncludeGL || !strings.HasPrefix(chrom, "GL") {
				chromNames = append(chromNames, chrom)
				if len(names) < maxSamples {
					charts = append(charts, c)
					if err := plotDepths(depths, names, chrom, cli.Prefix); err != nil {
						panic(err)
					}
				} else {
					tmp := chartjs.XFloatFormat
					chartjs.XFloatFormat = "%.2f"
					c.Options.Legend = &chartjs.Legend{Display: types.False}
					saveCharts(fmt.Sprintf("%s-indexcov-%s-roc.html", cli.Prefix, chrom), "", c)
					chartjs.XFloatFormat = tmp
				}
			}
			if len(charts) > 1 {
				charts[len(charts)-1].Options.Legend = &chartjs.Legend{Display: types.False}
			}
		}
	}
	return charts, sexes, offs, pca8, chromNames
}

func pca(pca8 [][]uint8, samples []string) (*mat64.Dense, []chartjs.Chart, string) {
	t := time.Now()
	mat := mat64.NewDense(len(pca8), len(pca8[0]), nil)
	row := make([]float64, len(pca8[0]))
	for i := 0; i < len(pca8); i++ {
		for j, v := range pca8[i] {
			row[j] = float64(v)
		}
		mat.SetRow(i, row)
	}
	var pc stat.PC
	if ok := pc.PrincipalComponents(mat, nil); !ok {
		panic("indexcov: error with principal components")
	}

	k := 5
	vars := pc.Vars(nil)
	floats.Scale(1/floats.Sum(vars), vars)
	if len(vars) < k {
		k = len(vars)
		log.Printf("got: %d, principal components", len(vars))
		if k < 3 {
			log.Printf("indexcov: %d principal components, not plotting", k)
			return nil, nil, ""
		}
	}
	vars = vars[:k]

	var proj mat64.Dense
	proj.Mul(mat, pc.Vectors(nil).Slice(0, len(pca8[0]), 0, k))
	pcaPlots, customjs := plotPCA(&proj, samples, vars)

	log.Printf("indexcov: completed PCA in: %.3f seconds", time.Since(t).Seconds())
	return &proj, pcaPlots, customjs
}

// write an index.html and a ped file. includes the PC projections and inferred sexes.
func writeIndex(sexes map[string][]float64, counts []*counter, keys []string, samples []string, prefix string, pca8 [][]uint8, chromNames []string) {
	if len(sexes) == 0 {
		return
	}
	for _, k := range keys {
		if _, ok := sexes[k]; !ok {
			fmt.Printf("chromosome %s not found. not writing ped\n", k)
			os.Exit(1)
		}
	}
	pcs, pcaPlots, pcajs := pca(pca8, samples)
	binChart, binjs := plotBins(counts, samples)

	sexes["_inferred"] = make([]float64, len(sexes[keys[0]]))
	f, err := os.Create(fmt.Sprintf("%s-indexcov.ped", prefix))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	hdr := make([]string, len(keys), len(keys)+7)
	for i, k := range keys {
		hdr[i] = "CN" + k
	}
	hdr = append(hdr, []string{"bins.out", "bins.lo", "bins.hi", "bins.in", "p.out"}...)
	if pcs != nil {
		hdr = append(hdr, "PC1\tPC2\tPC3\tPC4\tPC5")
	}

	fmt.Fprintf(f, "#family_id\tsample_id\tpaternal_id\tmaternal_id\tsex\tphenotype\t%s\n", strings.Join(hdr, "\t"))
	tmpl := "unknown\t%s\t-9\t-9\t%d\t-9\t"
	for i, s := range samples {
		inferred := int(0.5 + sexes[keys[0]][i])
		fmt.Fprintf(f, tmpl, s, inferred)
		sexes["_inferred"][i] = float64(inferred)
		s := make([]string, 0, len(keys)+4)
		for _, k := range keys {
			s = append(s, fmt.Sprintf("%.2f", sexes[k][i]))
		}
		cnt := counts[i]
		s = append(s, []string{
			fmt.Sprintf("%d", cnt.out),
			fmt.Sprintf("%d", cnt.low),
			fmt.Sprintf("%d", cnt.hi),
			fmt.Sprintf("%d", cnt.in),
			fmt.Sprintf("%.2f", float64(cnt.out)/float64(cnt.in)),
		}...)
		if pcs != nil {
			s = append(s,
				fmt.Sprintf("%.2f", pcs.At(i, 0)),
				fmt.Sprintf("%.2f", pcs.At(i, 1)),
				fmt.Sprintf("%.2f", pcs.At(i, 2)),
				fmt.Sprintf("%.2f", pcs.At(i, 3)),
				fmt.Sprintf("%.2f", pcs.At(i, 4)))
		}

		fmt.Fprintln(f, strings.Join(s, "\t"))
	}
	var sexChart *chartjs.Chart
	var sexjs string

	if len(keys) > 1 {
		sexChart, sexjs, err = plotSex(sexes, keys[:2], samples)
		if err != nil {
			panic(err)
		}
	}
	wtr, err := os.Create(fmt.Sprintf("%s-indexcov-index.html", cli.Prefix))
	if err != nil {
		panic(err)
	}

	chartMap := map[string]interface{}{"pcajs": template.JS(pcajs), "pcbjs": template.JS(pcajs),
		"template": chartTemplate, "pca": pcaPlots[0],
		"pcb": pcaPlots[1], "sex": *sexChart, "sexjs": template.JS(sexjs),
		"bin": binChart, "binjs": template.JS(binjs),
		"prefix": filepath.Base(prefix), "chroms": chromNames}
	chartMap["many"] = len(samples) > maxSamples
	if err := chartjs.SaveCharts(wtr, chartMap, chartjs.Chart{}); err != nil {
		panic(err)
	}
	wtr.Close()
}

// GetCN returns an float per sample estimating the number of copies of that chromosome.
// It is a very crude estimate, but that's what indexcov is and it tends to work well.
func GetCN(depths [][]float32) []float64 {
	if depths == nil {
		return nil
	}
	meds := make([]float64, 0, len(depths))
	for _, d := range depths {
		tmp := make([]float32, 0, len(d))
		for _, dp := range d {
			// exclude sites that are exactly 0 as these are the centromere.
			if dp != 0 {
				tmp = append(tmp, dp)
			}
		}
		if len(tmp) > 0 {
			sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
			med := float64(float32(Ploidy) * tmp[int(float64(len(tmp))*0.5)])
			meds = append(meds, med)
		} else {
			meds = append(meds, -1)
		}
	}
	return meds
}

func saveCharts(path string, customjs string, charts ...chartjs.Chart) {
	if len(charts) == 0 {
		return
	}
	wtr, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer wtr.Close()
	if err := chartjs.SaveCharts(wtr, map[string]interface{}{"height": 400, "width": 400, "custom": template.JS(customjs)}, charts...); err != nil {
		panic(err)
	}
}

func getROCs(counts [][]int) [][]float32 {
	rocs := make([][]float32, len(counts))

	for i, scount := range counts {
		rocs[i] = CountsROC(scount)
	}
	return rocs

}

func writeROCs(counts [][]int, names []string, chrom string, prefix string, fh io.Writer) chartjs.Chart {
	rocs := getROCs(counts)
	chart, err := plotROCs(rocs, names, chrom)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(fh, "#chrom\tcov\t%s\n", strings.Join(names, "\t"))
	nSamples := len(names)

	vals := make([]string, nSamples)

	for i := 0; i < slots; i++ {
		for k := 0; k < nSamples; k++ {
			vals[k] = fmt.Sprintf("%.2f", rocs[k][i])
		}
		fmt.Fprintf(fh, "%s\t%.2f\t%s\n", chrom, float64(i)/(slots*slotsMid), strings.Join(vals, "\t"))
	}
	return chart
}

func depthsFor(depths [][]float32, i int) string {
	s := make([]string, len(depths))
	for j := 0; j < len(depths); j++ {
		if i >= len(depths[j]) {
			s[j] = "0"
		} else {
			s[j] = fmt.Sprintf("%.3g", depths[j][i])
		}
	}
	return strings.Join(s, "\t")
}

type counter struct {
	// count of sites outside of (0.85, 1.15)
	out int
	// count of sites below 0.15
	low int
	// count of sites above 1.15
	hi int
	// count of sites inside of (0.85, 1.15)
	in int
}

// count values in or out of expected range of ~1.
func (c *counter) count(depths []float32, n int) {
	var i int
	for ; i < len(depths); i++ {
		if depths[i] < 0.85 || depths[i] > 1.15 {
			c.out++
			if depths[i] > 1.15 {
				c.hi++
			} else if depths[i] < 0.15 {
				c.low++
			}
		} else {
			c.in++
		}
	}
	c.out += n - i
	c.low += n - i
}
