// Minimal hand-built Capture fixtures for the verifier unit tests. NOT a test
// file (no `.test.ts`) — a shared builder so the tests read declaratively.

import type {
  AriaNode,
  ApiResponseItem,
  Capture,
  CapturePair,
  ServerTimeFrame,
} from '../../support/types'

export function frame(over: Partial<ServerTimeFrame> = {}): ServerTimeFrame {
  return {
    currentTime: '2026-07-12T15:48:12Z',
    isAccelerated: true,
    accelerationFactor: 60,
    baseTime: '2026-07-09T05:52:58Z',
    environment: 'testing',
    ...over,
  }
}

export function root(children: AriaNode[]): AriaNode {
  return { role: 'root', children }
}

export function apiItem(over: Partial<ApiResponseItem> = {}): ApiResponseItem {
  return {
    method: 'GET',
    requestUrl: '/api/v1/contacts',
    query: {},
    status: 200,
    body: null,
    ...over,
  }
}

let seqCounter = 0

export function cap(over: Partial<Capture> & { behaviors: string[] }): Capture {
  return {
    captureFormatVersion: 1,
    captureGeneratorVersion: 1,
    tour: 'contacts',
    seq: ++seqCounter,
    note: '',
    url: '/contacts',
    pair: null,
    serverTime: frame(),
    aria: root([]),
    apiResponses: {},
    dialogs: [],
    ...over,
  }
}

export function pair(id: string, role: string): CapturePair {
  return { id, role }
}
