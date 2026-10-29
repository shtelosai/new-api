import assert from 'node:assert/strict'
import { test } from 'node:test'

import { parseTiersFromExpr } from './billing-expr'
import {
  evalExprLocally,
  generateExprFromVisualConfig,
  normalizeVisualTier,
  tryParseVisualConfig,
  type ExtraTokenValues,
} from './tier-expr'

test('12h cache price survives visual editing and pricing display', () => {
  const config = {
    tiers: [
      normalizeVisualTier({
        label: 'base',
        input_unit_cost: 2,
        output_unit_cost: 10,
        cache_read_unit_cost: 0.2,
        cache_create_unit_cost: 2.5,
        cache_create_12h_unit_cost: 5,
      }),
    ],
  }
  const expr = generateExprFromVisualConfig(config)
  assert.ok(expr.includes('cc12h * 5'))
  assert.equal(
    tryParseVisualConfig(expr)?.tiers[0].cache_create_12h_unit_cost,
    5
  )
  assert.equal(parseTiersFromExpr(expr)[0].cacheCreate12hPrice, 5)
})

test('12h estimator agrees with backend independent charge examples', () => {
  const tokens: ExtraTokenValues = {
    cacheReadTokens: 4000,
    cacheCreateTokens: 3000,
    cacheCreate1hTokens: 0,
    cacheCreate12hTokens: 4000,
    imageTokens: 0,
    imageOutputTokens: 0,
    audioInputTokens: 0,
    audioOutputTokens: 0,
  }
  for (const expr of [
    'tier("base", p * 2 + c * 10 + cr * 0.2 + cc * 2.5 + cc12h * 5)',
    'tier("base", p * 2 + c * 10 + cr * 0.2 + cc * 2.5)',
  ]) {
    const result = evalExprLocally(expr, 1000, 200, tokens)
    assert.equal(result.error, null)
    assert.equal(result.cost, 32300)
  }
  const result = evalExprLocally(
    'len > 11999 ? tier("long", p) : tier("short", 0)',
    1000,
    200,
    tokens
  )
  assert.equal(result.matchedTier, 'long')
  assert.equal(result.cost, 16000)
})
