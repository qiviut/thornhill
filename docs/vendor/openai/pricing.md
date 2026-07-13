# Pricing

<style
  is:global
  set:html={`
    article table th,
    article table td {
      font-size: 14px;
    }

    :root {
      --pricing-section-spacing: 60px;
    }

    @media (min-width: 768px) {
      .pricing-switcher-layout {
        display: grid;
        grid-template-columns: minmax(0, 1fr) auto;
        grid-template-rows: auto auto;
        column-gap: 1.5rem;
        align-items: start;
      }

      .pricing-switcher-layout .content-switcher-root {
        display: contents;
      }

      .pricing-switcher-layout .pricing-switcher-header {
        grid-column: 1;
        grid-row: 1;
        align-self: start;
      }

      .pricing-switcher-layout .content-switcher-selector {
        grid-column: 2;
        grid-row: 1;
        margin-bottom: 0;
        margin-top: 0;
        align-self: start;
      }

      .pricing-switcher-layout .content-switcher-panes {
        grid-column: 1 / -1;
        grid-row: 2;
      }

      .pricing-switcher-layout-with-notice .pricing-switcher-notice {
        grid-column: 1 / -1;
        grid-row: 2;
      }

      .pricing-switcher-layout-with-notice .content-switcher-panes {
        grid-row: 3;
      }
    }

    .pricing-section-heading .anchor-heading-wrapper {
      margin-top: 0;
      margin-bottom: 0;
    }

    .pricing-section-heading .anchor-heading {
      margin-top: 0;
      margin-bottom: 0;
    }

    .pricing-section-heading .anchor-heading > p {
      margin: 0;
    }

    .pricing-section-heading .anchor-heading {
      display: block;
    }

    .pricing-switcher-header {
      display: flex;
      flex-direction: column;
    }

    .pricing-section-meta,
    .pricing-switcher-meta {
      display: block;
      margin-top: 4px;
      font-size: 14px;
      line-height: 20px;
      color: var(--color-text-secondary);
    }

    .pricing-switcher-subheading {
      display: block;
      margin-top: 4px;
      font-size: 16px;
      line-height: 24px;
      color: var(--color-text-primary);
    }

    .pricing-section-tip {
      margin-top: 16px;
      margin-bottom: 20px;
    }

    .pricing-subsection {
      margin-top: var(--pricing-section-spacing);
    }

    .pricing-switcher-layout + .pricing-switcher-layout {
      margin-top: var(--pricing-section-spacing);
    }

    .pricing-multimodal-subsection {
      margin-top: 28px;
    }

    .pricing-switcher-layout.pricing-multimodal-subsection + .pricing-switcher-layout.pricing-multimodal-subsection {
      margin-top: 28px;
    }
  `}
/>
<div id="latest-models" className="pricing-switcher-layout">
  <div className="pricing-switcher-header pricing-section-heading">
    

Flagship models


    <div className="pricing-switcher-subheading">Our latest models</div>
    <small className="pricing-switcher-meta">Prices per 1M tokens.</small>
  </div>

  

<div data-content-switcher-pane data-value="standard">
      <div class="hidden">Standard</div>

      <TextTokenPricingTables
        client:load
        tier="standard"
        latestSectionLabel={null}
        allModelsFootnote={pricingHtml(
          'Regional processing (data residency) endpoints are charged a 10% uplift for models released on or after March 5, 2026, that are eligible for data residency. See our <a href="/api/docs/guides/your-data">Your data</a> guide for supported regions and processing details. <a href="/api/docs/guides/amazon-bedrock">OpenAI models in Amazon Bedrock</a> are billed through AWS and may differ from direct OpenAI pricing.'
        )}
        rows={[
          ["gpt-5.5 (<272K context length)", 5, 0.5, 30],
          ["gpt-5.5-pro (<272K context length)", 30, "", 180],
          ["gpt-5.4 (<272K context length)", 2.5, 0.25, 15],
          ["gpt-5.4-mini", 0.75, 0.075, 4.5],
          ["gpt-5.4-nano", 0.2, 0.02, 1.25],
          ["gpt-5.4-pro (<272K context length)", 30, "", 180],
          ["gpt-5.2", 1.75, 0.175, 14],
          ["gpt-5.2-pro", 21, "-", 168],
          ["gpt-5.1", 1.25, 0.125, 10],
          ["gpt-5", 1.25, 0.125, 10],
          ["gpt-5-mini", 0.25, 0.025, 2],
          ["gpt-5-nano", 0.05, 0.005, 0.4],
          ["gpt-5-pro", 15, null, 120],
          ["gpt-4.1", 2, 0.5, 8],
          ["gpt-4.1-mini", 0.4, 0.1, 1.6],
          ["gpt-4.1-nano", 0.1, 0.025, 0.4],
          ["gpt-4o", 2.5, 1.25, 10],
          ["gpt-4o-2024-05-13", 5, null, 15],
          ["gpt-4o-mini", 0.15, 0.075, 0.6],
          ["o1", 15, 7.5, 60],
          ["o1-pro", 150, null, 600],
          ["o3-pro", 20, null, 80],
          ["o3", 2, 0.5, 8],
          ["o4-mini", 1.1, 0.275, 4.4],
          ["o3-mini", 1.1, 0.55, 4.4],
          ["o1-mini", 1.1, 0.55, 4.4],
          ["gpt-4-turbo-2024-04-09", 10, null, 30],
          ["gpt-4-0125-preview", 10, null, 30],
          ["gpt-4-1106-preview", 10, null, 30],
          ["gpt-4-1106-vision-preview", 10, null, 30],
          ["gpt-4-0613", 30, null, 60],
          ["gpt-4-0314", 30, null, 60],
          ["gpt-4-32k", 60, null, 120],
          ["gpt-3.5-turbo", 0.5, null, 1.5],
          ["gpt-3.5-turbo-0125", 0.5, null, 1.5],
          ["gpt-3.5-turbo-1106", 1, null, 2],
          ["gpt-3.5-turbo-0613", 1.5, null, 2],
          ["gpt-3.5-0301", 1.5, null, 2],
          ["gpt-3.5-turbo-instruct", 1.5, null, 2],
          ["gpt-3.5-turbo-16k-0613", 3, null, 4],
          ["davinci-002", 2, null, 2],
          ["babbage-002", 0.4, null, 0.4],
        ]}
      />
    </div>
    <div data-content-switcher-pane data-value="batch" hidden>
      <div class="hidden">Batch</div>

      <TextTokenPricingTables
        client:load
        tier="batch"
        latestSectionLabel={null}
        allModelsFootnote={pricingHtml(
          'Regional processing (data residency) endpoints are charged a 10% uplift for models released on or after March 5, 2026, that are eligible for data residency. See our <a href="/api/docs/guides/your-data">Your data</a> guide for supported regions and processing details.'
        )}
        rows={[
          ["gpt-5.5 (<272K context length)", 2.5, 0.25, 15],
          ["gpt-5.5-pro (<272K context length)", 15, "", 90],
          ["gpt-5.4 (<272K context length)", 1.25, 0.13, 7.5],
          ["gpt-5.4-mini", 0.375, 0.0375, 2.25],
          ["gpt-5.4-nano", 0.1, 0.01, 0.625],
          ["gpt-5.4-pro (<272K context length)", 15, "", 90],
          ["gpt-5.2", 0.875, 0.0875, 7],
          ["gpt-5.2-pro", 10.5, "-", 84],
          ["gpt-5.1", 0.625, 0.0625, 5],
          ["gpt-5", 0.625, 0.0625, 5],
          ["gpt-5-mini", 0.125, 0.0125, 1],
          ["gpt-5-nano", 0.025, 0.0025, 0.2],
          ["gpt-5-pro", 7.5, "-", 60],
          ["gpt-4.1", 1, "-", 4],
          ["gpt-4.1-mini", 0.2, "-", 0.8],
          ["gpt-4.1-nano", 0.05, "-", 0.2],
          ["gpt-4o", 1.25, "-", 5],
          ["gpt-4o-2024-05-13", 2.5, null, 7.5],
          ["gpt-4o-mini", 0.075, "-", 0.3],
          ["o1", 7.5, "-", 30],
          ["o1-pro", 75, "-", 300],
          ["o3-pro", 10, "-", 40],
          ["o3", 1, "-", 4],
          ["o4-mini", 0.55, "-", 2.2],
          ["o3-mini", 0.55, "-", 2.2],
          ["o1-mini", 0.55, "-", 2.2],
          ["gpt-4-turbo-2024-04-09", 5, null, 15],
          ["gpt-4-0125-preview", 5, null, 15],
          ["gpt-4-1106-preview", 5, null, 15],
          ["gpt-4-1106-vision-preview", 5, null, 15],
          ["gpt-4-0613", 15, null, 30],
          ["gpt-4-0314", 15, null, 30],
          ["gpt-4-32k", 30, null, 60],
          ["gpt-3.5-turbo-0125", 0.25, null, 0.75],
          ["gpt-3.5-turbo-1106", 1, null, 2],
          ["gpt-3.5-turbo-0613", 1.5, null, 2],
          ["gpt-3.5-0301", 1.5, null, 2],
          ["gpt-3.5-turbo-16k-0613", 1.5, null, 2],
          ["davinci-002", 1, null, 1],
          ["babbage-002", 0.2, null, 0.2],
        ]}
      />
    </div>
    <div data-content-switcher-pane data-value="flex" hidden>
      <div class="hidden">Flex</div>

      <TextTokenPricingTables
        client:load
        tier="flex"
        latestSectionLabel={null}
        allModelsFootnote={pricingHtml(
          'Regional processing (data residency) endpoints are charged a 10% uplift for models released on or after March 5, 2026, that are eligible for data residency. See our <a href="/api/docs/guides/your-data">Your data</a> guide for supported regions and processing details.'
        )}
        rows={[
          ["gpt-5.5 (<272K context length)", 2.5, 0.25, 15],
          ["gpt-5.5-pro (<272K context length)", 15, "", 90],
          ["gpt-5.4 (<272K context length)", 1.25, 0.13, 7.5],
          ["gpt-5.4-mini", 0.375, 0.0375, 2.25],
          ["gpt-5.4-nano", 0.1, 0.01, 0.625],
          ["gpt-5.4-pro (<272K context length)", 15, "", 90],
          ["gpt-5.2", 0.875, 0.0875, 7],
          ["gpt-5.1", 0.625, 0.0625, 5],
          ["gpt-5", 0.625, 0.0625, 5],
          ["gpt-5-mini", 0.125, 0.0125, 1],
          ["gpt-5-nano", 0.025, 0.0025, 0.2],
          ["o3", 1, 0.25, 4],
          ["o4-mini", 0.55, 0.138, 2.2],
        ]}
      />
    </div>
    <div data-content-switcher-pane data-value="priority" hidden>
      <div class="hidden">Priority</div>

      <TextTokenPricingTables
        client:load
        tier="priority"
        latestSectionLabel={null}
        allModelsFootnote={pricingHtml(
          'Regional processing (data residency) endpoints are charged a 10% uplift for models released on or after March 5, 2026, that are eligible for data residency. See our <a href="/api/docs/guides/your-data">Your data</a> guide for supported regions and processing details.'
        )}
        rows={[
          ["gpt-5.5 (<272K context length)", 12.5, 1.25, 75],
          ["gpt-5.4 (<272K context length)", 5, 0.5, 30],
          ["gpt-5.4-mini", 1.5, 0.15, 9],
          ["gpt-5.2", 3.5, 0.35, 28],
          ["gpt-5.1", 2.5, 0.25, 20],
          ["gpt-5", 2.5, 0.25, 20],
          ["gpt-5-mini", 0.45, 0.045, 3.6],
          ["gpt-4.1", 3.5, 0.875, 14],
          ["gpt-4.1-mini", 0.7, 0.175, 2.8],
          ["gpt-4.1-nano", 0.2, 0.05, 0.8],
          ["gpt-4o", 4.25, 2.125, 17],
          ["gpt-4o-2024-05-13", 8.75, null, 26.25],
          ["gpt-4o-mini", 0.25, 0.125, 1],
          ["o3", 3.5, 0.875, 14],
          ["o4-mini", 2, 0.5, 8],
        ]}
      />
    </div>


</div>
<div className="pricing-switcher-layout">
  <div className="pricing-switcher-header pricing-section-heading">
    

Multimodal models


  </div>
</div>
<div className="pricing-subsection pricing-section-heading pricing-multimodal-subsection">
  

Realtime and audio generation models


</div>
<p className="pricing-section-meta">Prices per 1M tokens unless noted.</p>
<div className="pricing-subsection pricing-switcher-layout pricing-multimodal-subsection">
  <div className="pricing-switcher-header pricing-section-heading">
    

Image generation models


    <small className="pricing-switcher-meta">Prices per 1M tokens.</small>
  </div>

  

<div data-content-switcher-pane data-value="standard">
      <div class="hidden">Standard</div>

      <div
        className="pricing-section-meta"
        style={{ marginBottom: "12px" }}
        set:html={`For image generation cost estimates, use the <a href="/api/docs/guides/image-generation#calculating-costs">calculator</a> in the image generation guide.`}
      />
      </div>
    <div data-content-switcher-pane data-value="batch" hidden>
      <div class="hidden">Batch</div>

      <div
        className="pricing-section-meta"
        style={{ marginBottom: "12px" }}
        set:html={`For image generation cost estimates, use the <a href="/api/docs/guides/image-generation#calculating-costs">calculator</a> in the image generation guide.`}
      />
      </div>


</div>
<div className="pricing-subsection pricing-switcher-layout pricing-multimodal-subsection">
  <div className="pricing-switcher-header pricing-section-heading">
    

Video generation models


    <small className="pricing-switcher-meta">Prices per second.</small>
  </div>

  

<div data-content-switcher-pane data-value="standard">
      <div class="hidden">Standard</div>

      </div>
    <div data-content-switcher-pane data-value="batch" hidden>
      <div class="hidden">Batch</div>

      </div>


</div>
<div className="pricing-subsection pricing-section-heading pricing-multimodal-subsection">
  

Transcription models


</div>
<p className="pricing-section-meta">Prices per 1M tokens unless noted.</p>
<div className="pricing-subsection pricing-section-heading">
  

Tools


</div>
<small>+ Search content tokens billed at model rates.</small>"
          ),
        ],
        [
          "Image Web search (all models)",
          pricingHtml(
            "$10.00 / 1k calls<br /><small>+ Search content tokens billed at model rates.</small>"
          ),
        ],
        [
          pricingHtml(
            "Web search preview (reasoning models, including <code>gpt-5</code>, <code>o-series</code>)"
          ),
          pricingHtml(
            "$10.00 / 1k calls<br /><small>+ Search content tokens billed at model rates.</small>"
          ),
        ],
        [
          "Web search preview (non-reasoning models)",
          pricingHtml(
            "$25.00 / 1k calls<br /><small>+ Search content tokens are free.</small>"
          ),
        ],
      ],
    },
    {
      model: "Containers",
      rows: [
        [
          pricingHtml(
            '<span id="container-usage-pricing"></span>Hosted Shell and Code Interpreter'
          ),
          "1 GB $0.03, 4 GB $0.12, 16 GB $0.48, 64 GB $1.92 per 20-minute session per container.",
        ],
      ],
    },
    {
      model: "File search",
      rows: [
        ["Storage", "$0.10 / GB per day (1 GB free)"],
        ["Tool call", "$2.50 / 1k calls"],
      ],
    },
    {
      model: "Agent Kit",
      rows: [
        [
          "ChatKit file and image upload storage",
          "$0.10 / GB-day after 1 GB free per account per month",
        ],
      ],
    },
  ]}
/>
<div
  className="pricing-section-meta"
  style={{ marginTop: "16px" }}
  set:html={`Tokens used for built-in tools are billed at the chosen model's per-token rates. GB refers to binary gigabytes (also known as gibibytes), where 1 GB is 2^30 bytes. Web search content tokens are tokens retrieved from the search index and fed to the model alongside your prompt to generate an answer. For <code>gpt-4o-mini</code> and <code>gpt-4.1-mini</code> with the non-preview web search tool, search content tokens are billed as a fixed block of 8,000 input tokens per call. File search tool call pricing applies to the Responses API only. Container pricing includes <a href="/api/docs/guides/tools-shell#hosted-shell-quickstart">Hosted Shell</a> and <a href="/api/docs/guides/tools-code-interpreter">Code Interpreter</a>. Eligible container sessions will be billed by the minute, with a 5-minute minimum per session. Responses API, Chat Completions API, Realtime API, Batch API, and Assistants API are not priced separately. Tokens are billed at the chosen model's input and output rates.`}
/>
<div className="pricing-subsection pricing-switcher-layout">
  <div className="pricing-switcher-header pricing-section-heading">
    

Specialized models


    <small className="pricing-switcher-meta">Prices per 1M tokens.</small>
  </div>

  

<div data-content-switcher-pane data-value="standard">
      <div class="hidden">Standard</div>

      <small>Web search tool call charges also apply.</small>"
                ),
              ],
            ],
          },
          {
            model: "Deep research",
            rows: [
              ["o3-deep-research", 10, 2.5, 40],
              ["o4-mini-deep-research", 2, 0.5, 8],
            ],
          },
          {
            model: "Computer use",
            rows: [["computer-use-preview", 3, "-", 12]],
          },
          {
            model: "Embedding",
            rows: [
              ["text-embedding-3-small", 0.02, "-", "-"],
              ["text-embedding-3-large", 0.13, "-", "-"],
              ["text-embedding-ada-002", 0.1, "-", "-"],
            ],
          },
          {
            model: "Moderation",
            rows: [["omni-moderation-latest", "Free", "-", "-"]],
          },
        ]}
      />
    </div>
    <div data-content-switcher-pane data-value="batch" hidden>
      <div class="hidden">Batch</div>

      </div>
    <div data-content-switcher-pane data-value="priority" hidden>
      <div class="hidden">Priority</div>

      </div>


</div>
<div className="pricing-subsection pricing-switcher-layout pricing-switcher-layout-with-notice">
  <div className="pricing-switcher-header pricing-section-heading">
    

Finetuning


    <small className="pricing-switcher-meta">Prices per 1M tokens.</small>
  </div>
  <div className="pricing-switcher-notice">
    OpenAI is winding down the fine-tuning platform. The platform is no longer
      accessible to new users, but existing users of the fine-tuning platform
      will be able to create training jobs for the coming months.
      <br />
      All fine-tuned models will remain available for inference until their base
      models are deprecated. The full timeline is
      [here](https://developers.openai.com/api/docs/deprecations#update-to-openais-self-serve-fine-tuning).
  </div>

  

<div data-content-switcher-pane data-value="standard">
      <div class="hidden">Standard</div>

      </div>
    <div data-content-switcher-pane data-value="batch" hidden>
      <div class="hidden">Batch</div>

      </div>


</div>
<div
  className="pricing-section-meta"
  set:html={`Tokens used for model grading in reinforcement fine-tuning are billed at that model's per-token rate. Inference discounts are available if you enable data sharing when creating the fine-tune job. <a href="https://help.openai.com/en/articles/10306912-sharing-feedback-evaluation-and-fine-tuning-data-and-api-inputs-and-outputs-with-openai#h_c93188c569">Learn more</a>.`}
/>