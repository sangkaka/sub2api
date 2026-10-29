import { readFileSync, readdirSync, statSync } from 'node:fs'
import { basename, dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

const sourceRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')

// 上游（Wei-Shaw）自有文件保持上游实现不改写：其自带 spec
// （AdminAccessKeysSettings.spec.ts）依赖原生 multiple <select> 与 window.confirm，
// fork 侧改组件会连带重写上游 spec，每次同步上游都得重来。仅豁免这些文件。
const UPSTREAM_OWNED_NATIVE = new Set(['AdminAccessKeysSettings.vue'])

function collectProductionSources(dir: string): string[] {
  return readdirSync(dir).flatMap((entry) => {
    if (entry === '__tests__') return []

    const path = resolve(dir, entry)
    const stat = statSync(path)
    if (stat.isDirectory()) return collectProductionSources(path)
    if (!/\.(ts|vue)$/.test(entry) || entry.endsWith('.d.ts')) return []
    return [path]
  })
}

const productionSources = collectProductionSources(sourceRoot)

function stripComments(source: string): string {
  return source
    .replace(/<!--[\s\S]*?-->/g, '')
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[^:])\/\/.*$/gm, '$1')
}

describe('native browser controls', () => {
  it('uses shared Select instead of native select tags in production Vue files', () => {
    const offenders = productionSources
      .filter((path) => path.endsWith('.vue'))
      .filter((path) => !UPSTREAM_OWNED_NATIVE.has(basename(path)))
      .filter((path) => /<select(\s|>|\/)/.test(readFileSync(path, 'utf8')))

    expect(offenders).toEqual([])
  })

  it('does not use native browser dialog APIs in production source', () => {
    const nativeDialogCall = /\b(?:window\.)?(?:alert|confirm|prompt)\s*\(/
    const offenders = productionSources
      .filter((path) => !UPSTREAM_OWNED_NATIVE.has(basename(path)))
      .filter((path) => nativeDialogCall.test(stripComments(readFileSync(path, 'utf8'))))

    expect(offenders).toEqual([])
  })
})
