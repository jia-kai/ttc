# Research notebook

## Results and evidence

The sample mean is **4.0**. This sentence uses _emphasis_, **bold**,
~~strikethrough~~, and `inline code`. Visit [the local notes](README.md).

| Sample | Value | Unit |
| ------ | ----- | ---- |
| A      | 2     | a.u. |
| B      | 4     | a.u. |
| C      | 6     | a.u. |

### Lists

- Read the observations.
- Calculate the mean.
  - Preserve the measurement units.
  - Record the sample count.

1. Verify the fixture.
2. Inspect the tool records.

- [x] Deterministic input
- [x] Local mock provider
- [x] Kitty formula image rendering

> Reproducibility starts with inspectable inputs and command results. This is a
> second quotation line.

### Code

```python
values = [2, 4, 6]
average = sum(values) / len(values)
assert average == 4
```

### Math

Inline math: $\bar{x} = 4$ with $n = 3$ samples.

Short inline prose: **Mean** $\bar{x}=4$, _count_ $n=3$, and `a.u.` for μm-scale
observations.

Long inline prose: compare $\frac{1}{n}\sum_{i=1}^{n}x_i$ with **the reference**
$\frac{\sum_{i=1}^{n}(x_i-\bar{x})^2}{n-1}$, then follow
[the local notes](README.md) and record _uncertainty_ in `a.u.`; the surrounding
words should wrap around each intact formula in a narrow pane, including Unicode
labels α, β, and 界.

#### Mixed-format math table

| Quantity $q$     | Formula and context                                                       | Reading                 |
| ---------------- | ------------------------------------------------------------------------- | ----------------------- |
| **Mean**         | $\bar{x}=4$ and $n=3$; _samples_ in `a.u.`, [notes](README.md), μm / 界   | Two inline formulas     |
| _Fraction_       | Before $\frac{\sum_{i=1}^{n}(x_i-\bar{x})^2}{n-1}$ after **variance**     | Tall atomic formula     |
| Matrix           | $\begin{pmatrix}1 & 2 \\ 3 & 4\end{pmatrix}$ with _basis_ $e_1$ and `2×2` | Multiple terminal rows  |
| Explicit display | $$x$$ beside **bold**, _emphasis_, and `code`                             | Single-line source cell |

These are ordinary GFM table rows: cells can occupy multiple terminal rows, and
each formula fits its actual cell width. No new table syntax is needed.

$$
\bar{x} = \frac{1}{n}\sum_{i=1}^{n} x_i = 4
$$

Math uses headless formula images placed with Kitty Unicode placeholders.
Unsupported TeX stays readable; tall inline formulas stay intact beside text,
occupying multiple terminal rows when needed. MathJax uses display-style
typesetting even for inline placement. Inspection renders this Markdown; export
preserves its source. Plain terminals retain the original TeX.

---

### Other useful cases

A very long identifier:
`research_sample_identifier_without_whitespace_012345678901234567890123456789`.

Unicode: α, β, Δ, μm, 界. Escaped punctuation: \*literal asterisks\*.

<!-- prettier-ignore -->
Term
: A small definition list entry.

The demo is complete. All 20 tool types have been exercised locally.
