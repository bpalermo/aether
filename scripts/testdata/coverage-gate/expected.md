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

