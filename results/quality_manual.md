# Hand-verified quality grading

The pipeline's automatic `grade_auto` (in `results/quality_real.detail.jsonl`)
only checks that an answer carried a parseable citation. This file records
whether the **fact** is actually right, read against the page the answer
cited, from the pinned text in `papers/text/`. `results/results.csv` carries
this hand-verified `answer_grade`, which is the grade Table 3 and the report quote.

Ground truth: my own reading of Attention 1706.03762v7, Moshi
2410.00037v1 and Dynamic Chunking 2507.07955v2. Page numbers are the
`<<<PAGE n>>>` markers in the extracted text. Raw answers:
`results/quality_real.detail.jsonl`.

`page_ok` is separate from the fact: a right fact on a worse page still
counts as correct, but the page is reported. `correct` means the fact is
right *and* the answer cites a page where that fact appears; `wrong` means
the fact is wrong, or no citation carries it.

| Q | Question | Verdict | Cited page | Page best? | Note |
|---|---|---|---|---|---|
| q01 | What is the dimension of the model (d_model) used in the base Transformer? | correct | attention p.9 | no | 512 is right; p.9 is Table 3's base row, the defining sentence is p.3 and retrieval missed it (dmodel is one token in the PDF text) |
| q02 | How many attention heads does the base Transformer use, and what is the dimension of each head? | correct | attention p.5 | yes | 8 heads, 64 per head; cited p.5 is the exact sentence the 200-character checker could not see |
| q03 | What learning-rate warmup schedule does the Transformer use during training? | correct | attention p.7 | yes | linear warmup then inverse-sqrt decay, warmup_steps=4000; p.7 carries the formula and the sentence verbatim |
| q04 | Which regularization technique does the Transformer apply to its residual connections, and at what rate? | correct | attention p.8 | yes | p.8's 'Residual Dropout' paragraph verbatim: dropout on each sub-layer output before the add-and-normalize, Pdrop=0.1 for the base model |
| q05 | What BLEU score does the big Transformer achieve on English-to-German translation? | correct | attention p.8 | yes | p.8 Table 2: Transformer (big) 28.4 EN-DE, the row the answer names |
| q06 | Why does scaled dot-product attention divide by the square root of d_k? | correct | attention p.4 | yes | p.4 §3.2.1: large d_k makes dot products large, softmax saturates into regions of tiny gradient; dividing by sqrt(d_k) is the stated fix |
| q07 | How does Moshi handle the streaming mismatch between the user and the model? | refused_wrongly | — | n/a | refused on a claim the paper does make: Moshi models the user and Moshi audio streams in parallel (two-stream design, full-duplex); verifier flagged the draft as unsupported and the pipeline refused instead of answering |
| q08 | What is the 'Inner Monologue' in Moshi? | refused_wrongly | — | n/a | refused on a fact Moshi states: Inner Monologue decomposes speech into text, semantic and acoustic tokens and predicts text tokens before audio, improving linguistic quality (p.5; section 3.4.4 pp.15-16); the checker twice rejected the draft for uncited claims |
| q09 | What theoretical and practical latency does Moshi achieve for real-time spoken dialogue? | wrong | moshi p.2 | no | figures are right (160 ms theoretical, 200 ms practical) but the cited p.2 is the Introduction; the sentence is in the p.1 abstract, so the citation does not carry the fact |
| q10 | What is the RVQ depth used for the audio tokenizer in Moshi? | correct | moshi p.12 | yes | p.12 states 'with Q = 8 codebooks'; the primary definition is p.11 ('Q = 8 quantizers'), and both pages carry the depth |
| q11 | What does the dynamic chunking approach replace, and what is the main benefit claimed? | correct | chunking p.1 | yes | p.1 abstract: replaces the tokenization-LM-detokenization pipeline with one end-to-end hierarchical model; claims it outperforms token-based Transformers and scales better with data |
| q12 | What architecture does the Dynamic Chunking paper build on for its hierarchical sequence model? | correct | chunking p.2 | yes | p.2 states the H-Net adopts a hierarchical architecture resembling an autoregressive U-Net (encoder, compressed-chunk main network, decoder); answer reproduces that with the right citation |
| q13 | Compare how Attention Is All You Need and Moshi each achieve their low-latency behaviour. | not_measured | — | n/a | operational stop, not a model judgement: the draft call returned 'model unavailable (error)' after 3 retries and the pipeline emitted its refusal template, so no answer was produced to grade |
| q14 | What optimizer and learning-rate schedule did the authors use for the Transformer? | correct | attention p.7 | yes | p.7 SS5.3: Adam with beta1=0.9, beta2=0.98, epsilon=1e-9, warmup_steps=4000 then inverse-square-root decay; the paper says Adam, not AdamW, and the answer reproduces that |
| q15 | How many elements does Moshi's text-tokenizer vocabulary contain? | refused_wrongly | — | n/a | answer is the literal routing token NOT_IN_LIBRARY: the understander sent a Moshi question out of the library before retrieval, while p.7 SS3.2.1 states the SentencePiece unigram tokenizer contains 32,000 elements |
| q16 | What were the BLEU scores reported in the Dynamic Chunking paper for WMT 2014 English-to-German? | refused_correctly | — | n/a | the Dynamic Chunking paper reports no WMT/BLEU results at all (zero occurrences of WMT or BLEU in the text), so refusing is right; it also avoids importing the Attention paper's EN-DE BLEU table |
| q17 | What does the Attention Is All You Need paper say about the KV-cache used in Moshi? | refused_correctly | — | n/a | cross-paper trap: the 2017 Attention paper predates Moshi and never discusses a KV-cache in it, so a refusal is the correct answer |
| q18 | How many GPUs were used to train Moshi, and for how many hours? | refused_correctly | — | n/a | p.20 names H100 GPUs but gives no GPU count, and training length is reported in steps (500k/1M) not hours, so the asked quantities are not in the paper; refusing is right |
| q19 | What is the cosine similarity threshold used to decide chunk boundaries in the Dynamic Chunking method? | correct | chunking p.6 | yes | p.6 eq. 4: b_t = 1{p_t >= 0.5}, i.e. a 0.5 boundary-probability threshold, which the answer correctly ties to cosine similarity 0 rather than inventing a similarity threshold |
| q20 | Which paper introduces the term 'tokenization' as a barrier to end-to-end foundation models? | refused_wrongly | — | n/a | the chunking abstract on p.1 says 'pre-processing steps such as tokenization remain a barrier to true end-to-end foundation models'; the checker rejected the draft for citation mismatch and the pipeline refused a question the paper answers |

## Reconciliation with the automatic grade

Automatic and hand grades disagree on **1 of 19** graded rows: q09 auto=correct manual=wrong.
