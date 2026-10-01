# Research notebook

## Results and evidence

The sample mean is **4.0**. This sentence uses *emphasis*, **bold**,
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

> Reproducibility starts with inspectable inputs and command results.
> This is a second quotation line.

### Code

```python
values = [2, 4, 6]
average = sum(values) / len(values)
assert average == 4
```

### Math

Inline math: $\bar{x} = 4$ with $n = 3$ samples.

$$
\bar{x} = \frac{1}{n}\sum_{i=1}^{n} x_i = 4
$$

Math uses headless formula images placed with Kitty Unicode placeholders.
Unsupported TeX stays readable; large inline equations can become blocks.
Inspect this message to see its exact source. Plain terminals retain the original TeX.

---

### Other useful cases

A very long identifier: `research_sample_identifier_without_whitespace_012345678901234567890123456789`.

Unicode: α, β, Δ, μm, 界. Escaped punctuation: \*literal asterisks\*.

Term
: A small definition list entry.

The demo is complete. All 18 tool types have been exercised locally.
