package ai

// SystemEnvVarsPrompt instructs the model to extract env-var requirements only.
// The output is consumed by the frontend as a Markdown list; the model is told
// not to fabricate beyond what the files suggest.
const SystemEnvVarsPrompt = `You are analyzing a GitHub repository that will be deployed as a sandboxed API.
List the environment variables this application requires at runtime. Only include
variables that are genuinely referenced by or strongly implied by the files shown
(DATABASE_URL, API tokens, PORT, secrets, etc.). For each, give the variable name,
a 1-line description, and whether it is required (true/false).
Format as a JSON object: {"env_vars": [{"name": "...", "description": "...", "required": true/false}]}.
Do not include any text outside the JSON object.`

// SystemOpenAPIPrompt instructs the model to produce an OpenAPI skeleton from
// route-like lines. Output is YAML, kept minimal and editable.
const SystemOpenAPIPrompt = `You are generating a starting OpenAPI 3.0 skeleton for a detected backend API.
Use the route definitions and code snippets shown. Produce a YAML document with
openapi: 3.0.3, a basic info block (title, version 0.1.0), and a paths section
with one operation per discovered route (method, summary, and where evidence
exists, request/response shapes). Keep it concise — this is a skeleton a human
will edit. Output YAML only, no prose before or after.`