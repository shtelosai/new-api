import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { usageLogSchema } from '../data/schema'

describe('usage log schema', () => {
  test('preserves the optional Twork username returned for admin logs', () => {
    const log = usageLogSchema.parse({
      id: 1,
      user_id: 1,
      created_at: 1,
      type: 2,
      content: '',
      twork_username: 'alice',
    })

    assert.equal(log.twork_username, 'alice')
  })
})
