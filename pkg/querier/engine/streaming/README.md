# Streaming PromQL engine

This file contains a brief overview of the internals of the streaming PromQL engine.

For an introduction to the engine itself and the problems it tries to solve, check out [this PromCon 2023 talk](https://www.youtube.com/watch?v=3kM2Asj6hcg).

The goal of the engine is to allow evaluating queries over millions of series in a safe, performant and cost-effective way.
To allow this, the engine aims to ensure that peak memory consumption of queriers is not proportional to the number of series selected.
This will make it safe for operators to loosen the various query-related limits without risking the stability of their Mimir cluster or needing to devote enormous amounts of compute resources to queriers.

The key way the engine achieves this is by not loading all the input series into memory at once, and instead streaming them into memory when needed.

For example, let's say we're evaluating the query `sum by (environment) (some_metric{cluster="cluster-1"})`.

Prometheus' PromQL engine will first load all samples for all series selected by `some_metric{cluster="cluster-1"}` into memory.
It will then compute the sum for each unique value of `environment`.
At its peak, Prometheus' PromQL engine will hold all samples for all input series (from `some_metric{cluster="cluster-1"}) and all samples for all output series in memory at once.

The streaming engine here will instead execute the selector `some_metric{cluster="cluster-1"}` and gather the labels of all series returned.
With these labels, it will then compute all the possible output series for the `sum by (environment)` operation (ie. one output series per unique value of `environment`).
Having computed the output series, it will then begin reading series from the selector, one at a time, and update the running total for the appropriate output series.
At its peak, the streaming engine in this example will hold all samples for one input series and all samples for all output series in memory at once[^1],
a significant reduction compared to Prometheus' PromQL engine, particularly when the selector selects many series.

This idea of streaming can be applied to multiple levels as well. Imagine we're evaluating the query `max(sum by (environment) (some_metric{cluster="cluster-1"}))`.
In the streaming engine, once the result of each group series produced by `sum` is complete, it is passed to `max`, which can update its running maximum seen so far across all groups.
At its peak, the streaming engine will hold all samples for one input series, all samples for all incomplete `sum` group series, and the single incomplete `max` output series in memory at once.

## Internals

Within the streaming engine, a query is represented by a set of linked operators (one for each operation) that together form the query plan.

For example, the `max(sum by (environment) (some_metric{cluster="cluster-1"}))` example from before would have a query plan made up of three operators:

- The instant vector selector operator (`some_metric{cluster="cluster-1"}`)
- The `sum` aggregation operator (`sum by (environment) (...)`), which consumes series from the instant vector selector operator
- The `max` aggregation operator (`max (...)`), which consumes series from the `sum` aggregation operator

Visually, the plan looks like this:

```mermaid
flowchart TB
    IVS["`**instant vector selector**
    some_metric#123;cluster=#quot;cluster-1#quot;#125;`"]
    sum["`**sum aggregation**
    sum by (environment) (...)`"]
    max["`**max aggregation**
    max (...)`"]
    output((output))
    IVS --> sum
    sum --> max
    max --> output
```

Each of these operators satisfies the `InstantVectorOperator` interface, defined [here](./operator/operator.go).
The two key methods of this interface are `Series()` and `Next()`:

`Series()` returns the list of all series' labels that will be returned by the operator[^2].
In our example, the instant vector selector operator would return all the matching `some_metric` series, and the `sum` aggregation operator would return one series for each unique value of `environment`.

`Next()` is then called by the consuming operator to read each series' data, one series at a time.
In our example, the `sum` aggregation operator would call `Next()` on the instant vector selector operator to get the first series' data, then again to get the second series' data and so on.

Elaborating on the example from before, the overall query would proceed like this:

1. root of the query calls `Series()` on `max` aggregation operator
   1. `max` aggregation operator calls `Series()` on `sum` aggregation operator
      1. `sum` aggregation operator calls `Series()` on instant vector selector operator
         - instant vector selector operator issues `Select()` call, which retrieves labels from ingesters and store-gateways
      2. `sum` aggregation operator computes output series (one per unique value of `environment`) based on input series from instant vector selector
   2. `max` aggregation operator computes output series based on input series from `sum` aggregation operator
      - in this case, there's just one output series, given no grouping is being performed
2. root of the query calls `Next()` on `max` aggregation operator until all series have been returned
   1. `max` aggregation operator calls `Next()` on `sum` aggregation operator
      1. `sum` aggregation operator calls `Next()` on instant vector selector operator
         - instant vector selector returns samples for next series
      2. `sum` aggregation operator updates its running totals for the relevant output series
      3. if all input series have now been seen for the output series just updated, `sum` aggregation operator returns that output series and removes it from its internal state
      4. otherwise, it calls `Next()` again and repeats
   2. `max` aggregation operator updates its running maximum based on the series returned
   3. if all input series have been seen, `max` aggregation operator returns
   4. otherwise, it calls `Next()` again and repeats

[^1]:
    This isn't strictly correct, as chunks streaming will buffer chunks for some series in memory as they're received over the network, and it ignores the initial memory consumption caused by the non-streaming calls to `Series()`.
    But this applies equally to both engines when used in Mimir.

[^2]:
    This isn't done in a streaming fashion: all series' labels are loaded into memory at once.
    In a future iteration of the engine, `Series()` could be made streaming as well, but this is out of scope for now.