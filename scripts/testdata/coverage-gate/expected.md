## Coverage gate: FAILED

Total line coverage dropped by **20.000 points**, more than the **1.0** allowed.

| | Lines | Covered | Coverage |
| --- | ---: | ---: | ---: |
| Baseline: main at `0123abc` | 20 | 15 | 75.00% |
| This pull request | 20 | 11 | 55.00% |
| **Delta** | +0 | -4 | **-20.000 points** |

Gate: maximum drop 1.0 points (the default), no minimum. Only the total gates; the tables below show where a change came from.

If the drop is legitimate (deleting well-tested code lowers the percentage too), see docs/runbook.md, "Code coverage", "The gate failed".

### By component

| Component | Baseline lines | Baseline covered | Baseline | Lines | Covered | Coverage | Delta (points) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `agent` | 14 | 12 | 85.71% | 15 | 7 | 46.67% | -39.05 |
| `cni` | 4 | 2 | 50.00% | 4 | 3 | 75.00% | +25.00 |
| `edge` | - | - | - | 1 | 1 | 100.00% | new |
| `legacy` | 2 | 1 | 50.00% | - | - | - | removed |
| **Total** | 20 | 15 | 75.00% | 20 | 11 | 55.00% | **-20.00** |

### Changed files

| File | Baseline lines | Baseline covered | Baseline | Lines | Covered | Coverage | Delta (points) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `agent/a.go` | 10 | 8 | 80.00% | 10 | 7 | 70.00% | -10.00 |
| `agent/gone.go` | 4 | 4 | 100.00% | - | - | - | removed |
| `agent/new.go` | - | - | - | 5 | 0 | 0.00% | new |
| **3 file(s)** | 14 | 12 | 85.71% | 15 | 7 | 46.67% | -39.05 |

2 changed path(s) are in neither report (tests, non-Go files, generated code, or files excluded by build constraints).

### Largest per-file drops

The files whose covered-line count fell most between the baseline and this pull request, whatever the diff touched (a test disabled in a BUILD file changes no Go file). `removed`: the file is no longer in the report.

| File | Baseline covered / lines | Covered / lines | Covered lines |
| --- | ---: | ---: | ---: |
| `agent/gone.go` | 4 / 4 | removed | -4 |
| `agent/a.go` | 8 / 10 | 7 / 10 | -1 |
| `legacy/old.go` | 1 / 2 | removed | -1 |
| **3 file(s)** | | | **-6** |

