import { describe, expect, test } from 'bun:test'
import {
  checkCollisions,
  compileMatchPattern,
  emitMatchPattern,
  parseFile,
  serialize,
  type ModelRow,
} from './schema'

function row(partial: Partial<ModelRow> & Pick<ModelRow, 'modelName' | 'source' | 'sourceId'>): ModelRow {
  return { prices: { input: 1e-6, output: 2e-6 }, ...partial }
}

describe('emitMatchPattern', () => {
  test('venice row escapes the sourceId and allows an optional venice/ prefix', () => {
    const r = row({ modelName: 'qwen3-235b', source: 'venice', sourceId: 'qwen3-235b' })
    expect(emitMatchPattern(r)).toBe('(?i)^(venice\\/)?qwen3\\-235b$')
  })

  test('open-router row is an escaped exact match of the model string', () => {
    const r = row({ modelName: 'gpt-5.5', source: 'open-router', sourceId: 'openai/gpt-5.5' })
    const pattern = emitMatchPattern(r)
    expect(pattern).toBe('(?i)^gpt\\-5\\.5$')
    // The escaped '.' must not act as "any character" — an unescaped pattern would
    // wrongly match "gpt-515".
    const re = compileMatchPattern(pattern)
    expect(re.test('gpt-515')).toBe(false)
    expect(re.test('gpt-5.5')).toBe(true)
  })
})

describe('compileMatchPattern self-match sanity', () => {
  test('every row emitted pattern matches its own model string', () => {
    const rows = [
      row({ modelName: 'qwen3-235b', source: 'venice', sourceId: 'qwen3-235b' }),
      row({ modelName: 'gpt-5.6-luna', source: 'open-router', sourceId: 'openai/gpt-5.6-luna' }),
    ]
    for (const r of rows) {
      expect(compileMatchPattern(emitMatchPattern(r)).test(r.modelName)).toBe(true)
    }
  })
})

describe('checkCollisions', () => {
  test('two rows whose patterns both match one model string throw, naming both', () => {
    const rows = [
      row({ modelName: 'qwen3-235b', source: 'venice', sourceId: 'qwen3-235b' }),
      row({ modelName: 'venice/qwen3-235b', source: 'open-router', sourceId: 'someone/venice-qwen3-235b' }),
    ]
    expect(() => checkCollisions(rows)).toThrow(/qwen3-235b/)
    expect(() => checkCollisions(rows)).toThrow(/venice\/qwen3-235b/)
  })

  test('non-colliding rows pass silently', () => {
    const rows = [
      row({ modelName: 'gpt-5.5', source: 'open-router', sourceId: 'openai/gpt-5.5' }),
      row({ modelName: 'gpt-5.6-luna', source: 'open-router', sourceId: 'openai/gpt-5.6-luna' }),
    ]
    expect(() => checkCollisions(rows)).not.toThrow()
  })
})

describe('serialize', () => {
  test('sorts by modelName, 2-space indent, trailing newline', () => {
    const file = {
      models: [
        row({ modelName: 'zeta', source: 'open-router', sourceId: 'openai/zeta' }),
        row({ modelName: 'alpha', source: 'open-router', sourceId: 'openai/alpha' }),
      ],
    }
    const out = serialize(file)
    expect(out.endsWith('\n')).toBe(true)
    expect(out.includes('  "models"')).toBe(true)
    const alphaIdx = out.indexOf('alpha')
    const zetaIdx = out.indexOf('zeta')
    expect(alphaIdx).toBeGreaterThan(-1)
    expect(zetaIdx).toBeGreaterThan(-1)
    expect(alphaIdx).toBeLessThan(zetaIdx)
  })

  test('serialize(parseFile(JSON.parse(serialize(x)))) is byte-for-byte stable', () => {
    const file = {
      models: [
        row({ modelName: 'gpt-5.5', source: 'open-router', sourceId: 'openai/gpt-5.5' }),
        row({
          modelName: 'gpt-5.4-mini',
          source: 'open-router',
          sourceId: 'openai/gpt-5.4-mini',
          prices: { input: 7.5e-7, output: 4.5e-6, cachedInput: 7.5e-8 },
        }),
      ],
    }
    const once = serialize(file)
    const roundTripped = serialize(parseFile(JSON.parse(once)))
    expect(roundTripped).toBe(once)
  })
})

describe('parseFile validation', () => {
  test('accepts an optional cachedInput', () => {
    const parsed = parseFile({
      models: [
        {
          modelName: 'gpt-5.4-mini',
          source: 'open-router',
          sourceId: 'openai/gpt-5.4-mini',
          prices: { input: 7.5e-7, output: 4.5e-6, cachedInput: 7.5e-8 },
        },
      ],
    })
    expect(parsed.models[0]?.prices.cachedInput).toBe(7.5e-8)
  })

  test('rejects an unknown source value', () => {
    expect(() =>
      parseFile({
        models: [
          { modelName: 'x', source: 'anthropic', sourceId: 'x', prices: { input: 1e-6, output: 1e-6 } },
        ],
      })
    ).toThrow(/source/)
  })

  test('rejects a missing sourceId', () => {
    expect(() =>
      parseFile({
        models: [{ modelName: 'x', source: 'venice', prices: { input: 1e-6, output: 1e-6 } }],
      })
    ).toThrow(/sourceId/)
  })

  test('rejects a non-numeric price', () => {
    expect(() =>
      parseFile({
        models: [
          {
            modelName: 'x',
            source: 'venice',
            sourceId: 'x',
            prices: { input: '1e-6', output: 1e-6 },
          },
        ],
      })
    ).toThrow(/input/)
  })
})
