export type ImportedCredential = Record<string, unknown> & { access_token?: string; refresh_token?: string }

export function parseAccountCredentials(text: string, mode = 'access_token'): ImportedCredential[] {
  text = text.replace(/^\uFEFF/, '').trim()
  const rows: ImportedCredential[] = []
  const value = (o: Record<string, unknown>, ...keys: string[]) => {
    for (const key of keys) if (typeof o[key] === 'string' && String(o[key]).trim()) return String(o[key]).trim()
    return ''
  }
  const walk = (input: unknown, depth = 0) => {
    if (depth > 12) return
    if (Array.isArray(input)) { input.forEach(v => walk(v, depth + 1)); return }
    if (!input || typeof input !== 'object') return
    const o = input as Record<string, unknown>
    const at = value(o, 'access_token', 'accessToken')
    const rt = value(o, 'refresh_token', 'refreshToken')
    if (at || rt) {
      const row: ImportedCredential = { access_token: at, refresh_token: rt }
      for (const key of ['email', 'type', 'plan_type', 'id_token']) if (value(o, key)) row[key] = value(o, key)
      if (!row.email && o.user && typeof o.user === 'object') row.email = value(o.user as Record<string, unknown>, 'email')
      rows.push(row)
      return
    }
    for (const key of ['accounts', 'items', 'data', 'results', 'credentials']) if (o[key]) walk(o[key], depth + 1)
  }
  if (text.startsWith('{') || text.startsWith('[')) {
    try { walk(JSON.parse(text)) } catch {
      // JSONL must consist entirely of complete JSON objects.
      for (const line of text.split(/\r?\n/).filter(l => l.trim())) walk(JSON.parse(line))
    }
  } else {
    for (const line of text.split(/\r?\n/).map(l => l.trim()).filter(l => l && !l.startsWith('#'))) {
      rows.push(mode === 'refresh_token' || line.startsWith('rt.') ? { refresh_token: line } : { access_token: line })
    }
  }
  const unique = new Map<string, ImportedCredential>()
  for (const row of rows) unique.set(String(row.refresh_token || row.access_token), row)
  if (!unique.size) throw new Error('未找到 AT 或 RT，请检查 JSON 的 access_token / refresh_token 键')
  if (unique.size > 50_000) throw new Error('单次最多导入 50000 个账号')
  return Array.from(unique.values())
}
