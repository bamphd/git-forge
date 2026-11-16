// The GraphQL surface: a small, self-contained executor for the fixed set of documents the
// conformance suite sends (gh's repository/issue/pull-request reads, introspection probes, and
// five mutations), plus JSON-array operation batching. No schema library, no code generation:
// a lexer, a recursive-descent parser for the GraphQL query language, and resolvers over the
// one store, so REST, git, GraphQL and MCP always read the same objects.
//
// Wire rules, as the suite asserts them: a valid document with an unknown field is HTTP 200
// with an `errors` array naming the field; an unknown repository is `data.repository = null`
// with no errors; a batch body (a JSON array of operation documents) answers a JSON array of
// envelopes in order. Authentication is the app layer's job; this module assumes a valid token.

import {
  Store,
  StoreError,
  VIEWER,
  globalCommentId,
  globalItemId,
  globalRepoId,
  trialNowIso,
  type Comment,
  type Item,
  type Repo,
} from './store.ts';

// --------------------------------------------------------------------------- lexer

type TokenKind = 'name' | 'int' | 'float' | 'string' | 'punct' | 'eof';

interface Token {
  readonly kind: TokenKind;
  readonly value: string;
  readonly position: number;
}

class GraphQLSyntaxError extends Error {}

const PUNCTUATORS = new Set(['!', '$', '(', ')', ':', '=', '@', '[', ']', '{', '}', '|', '&']);

function lex(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const push = (kind: TokenKind, value: string, position: number): void => {
    tokens.push({ kind, value, position });
  };
  while (i < source.length) {
    const ch = source[i] as string;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',' || ch === '﻿') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && source.slice(i, i + 3) === '...') {
      push('punct', '...', i);
      i += 3;
      continue;
    }
    if (PUNCTUATORS.has(ch)) {
      push('punct', ch, i);
      i += 1;
      continue;
    }
    if (ch === '"') {
      const start = i;
      if (source.slice(i, i + 3) === '"""') {
        const end = source.indexOf('"""', i + 3);
        if (end === -1) throw new GraphQLSyntaxError('Unterminated block string');
        push('string', source.slice(i + 3, end).replace(/\\"""/g, '"""'), start);
        i = end + 3;
        continue;
      }
      i += 1;
      let value = '';
      while (i < source.length) {
        const c = source[i] as string;
        if (c === '"') break;
        if (c === '\n') throw new GraphQLSyntaxError('Unterminated string');
        if (c === '\\') {
          const esc = source[i + 1] as string;
          const table: Record<string, string> = { '"': '"', '\\': '\\', '/': '/', b: '\b', f: '', n: '\n', r: '\r', t: '\t' };
          if (esc === 'u') {
            value += String.fromCharCode(Number.parseInt(source.slice(i + 2, i + 6), 16));
            i += 6;
          } else if (table[esc] !== undefined) {
            value += table[esc] as string;
            i += 2;
          } else {
            throw new GraphQLSyntaxError(`Invalid string escape \\${esc}`);
          }
          continue;
        }
        value += c;
        i += 1;
      }
      if (i >= source.length) throw new GraphQLSyntaxError('Unterminated string');
      push('string', value, start);
      i += 1;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      const start = i;
      const match = /^-?(?:0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(source.slice(i));
      if (!match) throw new GraphQLSyntaxError(`Invalid number at ${i}`);
      const text = match[0] as string;
      push(match[1] === undefined && match[2] === undefined ? 'int' : 'float', text, start);
      i += text.length;
      continue;
    }
    if (/[_A-Za-z]/.test(ch)) {
      const start = i;
      while (i < source.length && /[_A-Za-z0-9]/.test(source[i] as string)) i += 1;
      push('name', source.slice(start, i), start);
      continue;
    }
    throw new GraphQLSyntaxError(`Unexpected character ${JSON.stringify(ch)} at ${i}`);
  }
  push('eof', '', i);
  return tokens;
}

// --------------------------------------------------------------------------- parser

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: { name: string; value: ValueNode }[] };

interface ArgumentNode {
  readonly name: string;
  readonly value: ValueNode;
}

interface DirectiveNode {
  readonly name: string;
  readonly args: ArgumentNode[];
}

type SelectionNode =
  | { kind: 'field'; alias: string | null; name: string; args: ArgumentNode[]; directives: DirectiveNode[]; selectionSet: SelectionNode[] | null }
  | { kind: 'fragmentSpread'; name: string; directives: DirectiveNode[] }
  | { kind: 'inlineFragment'; typeCondition: string | null; directives: DirectiveNode[]; selectionSet: SelectionNode[] };

interface OperationNode {
  readonly operation: 'query' | 'mutation' | 'subscription';
  readonly name: string | null;
  readonly variables: { name: string; defaultValue: ValueNode | null }[];
  readonly selectionSet: SelectionNode[];
}

interface FragmentNode {
  readonly name: string;
  readonly typeCondition: string;
  readonly selectionSet: SelectionNode[];
}

interface DocumentNode {
  readonly operations: OperationNode[];
  readonly fragments: Map<string, FragmentNode>;
}

class Parser {
  private index = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.index] as Token;
  }

  private next(): Token {
    const token = this.tokens[this.index] as Token;
    if (token.kind !== 'eof') this.index += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new GraphQLSyntaxError(`Expected "${value}", got ${token.value || 'end of document'}`);
  }

  private expectName(value?: string): string {
    const token = this.next();
    if (token.kind !== 'name' || (value !== undefined && token.value !== value)) {
      throw new GraphQLSyntaxError(`Expected name ${value ?? ''}, got ${token.value || 'end of document'}`);
    }
    return token.value;
  }

  private skipIfPunct(value: string): boolean {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === value) {
      this.index += 1;
      return true;
    }
    return false;
  }

  parseDocument(): DocumentNode {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, FragmentNode>();
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === '{') {
        operations.push({ operation: 'query', name: null, variables: [], selectionSet: this.parseSelectionSet() });
      } else if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation' || token.value === 'subscription')) {
        const operation = this.next().value as 'query' | 'mutation' | 'subscription';
        let name: string | null = null;
        if (this.peek().kind === 'name') name = this.expectName();
        const variables = this.parseVariableDefinitions();
        this.parseDirectives();
        operations.push({ operation, name, variables, selectionSet: this.parseSelectionSet() });
      } else if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const name = this.expectName();
        this.expectName('on');
        const typeCondition = this.expectName();
        this.parseDirectives();
        fragments.set(name, { name, typeCondition, selectionSet: this.parseSelectionSet() });
      } else {
        throw new GraphQLSyntaxError(`Unexpected token ${token.value} at ${token.position}`);
      }
    }
    return { operations, fragments };
  }

  private parseVariableDefinitions(): { name: string; defaultValue: ValueNode | null }[] {
    const definitions: { name: string; defaultValue: ValueNode | null }[] = [];
    if (!this.skipIfPunct('(')) return definitions;
    while (!this.skipIfPunct(')')) {
      this.expectPunct('$');
      const name = this.expectName();
      this.expectPunct(':');
      this.parseType();
      let defaultValue: ValueNode | null = null;
      if (this.skipIfPunct('=')) defaultValue = this.parseValue();
      definitions.push({ name, defaultValue });
    }
    return definitions;
  }

  private parseType(): void {
    if (this.skipIfPunct('[')) {
      this.parseType();
      this.expectPunct(']');
    } else {
      this.expectName();
    }
    this.skipIfPunct('!');
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    while (!this.skipIfPunct('}')) {
      selections.push(this.parseSelection());
    }
    return selections;
  }

  private parseSelection(): SelectionNode {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === '...') {
      this.next();
      const next = this.peek();
      if (next.kind === 'name' && next.value !== 'on') {
        const name = this.expectName();
        const directives = this.parseDirectives();
        return { kind: 'fragmentSpread', name, directives };
      }
      let typeCondition: string | null = null;
      if (next.kind === 'name' && next.value === 'on') {
        this.next();
        typeCondition = this.expectName();
      }
      const directives = this.parseDirectives();
      return { kind: 'inlineFragment', typeCondition, directives, selectionSet: this.parseSelectionSet() };
    }
    let name = this.expectName();
    let alias: string | null = null;
    if (this.skipIfPunct(':')) {
      alias = name;
      name = this.expectName();
    }
    const args = this.parseArguments();
    const directives = this.parseDirectives();
    const selectionSet = this.peek().kind === 'punct' && this.peek().value === '{' ? this.parseSelectionSet() : null;
    return { kind: 'field', alias, name, args, directives, selectionSet };
  }

  private parseArguments(): ArgumentNode[] {
    const args: ArgumentNode[] = [];
    if (!this.skipIfPunct('(')) return args;
    while (!this.skipIfPunct(')')) {
      const name = this.expectName();
      this.expectPunct(':');
      args.push({ name, value: this.parseValue() });
    }
    return args;
  }

  private parseDirectives(): DirectiveNode[] {
    const directives: DirectiveNode[] = [];
    while (this.skipIfPunct('@')) {
      const name = this.expectName();
      directives.push({ name, args: this.parseArguments() });
    }
    return directives;
  }

  private parseValue(): ValueNode {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '$') return { kind: 'variable', name: this.expectName() };
    if (token.kind === 'int') return { kind: 'int', value: Number.parseInt(token.value, 10) };
    if (token.kind === 'float') return { kind: 'float', value: Number.parseFloat(token.value) };
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'punct' && token.value === '[') {
      const values: ValueNode[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) values.push(this.parseValue());
      this.next();
      return { kind: 'list', values };
    }
    if (token.kind === 'punct' && token.value === '{') {
      const fields: { name: string; value: ValueNode }[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields.push({ name, value: this.parseValue() });
      }
      this.next();
      return { kind: 'object', fields };
    }
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    throw new GraphQLSyntaxError(`Unexpected value token ${token.value}`);
  }
}

// --------------------------------------------------------------------------- execution

type JsonValue = string | number | boolean | null | JsonValue[] | { [key: string]: JsonValue };

/** A resolved object: its GraphQL type name plus the store record behind it. */
interface TypedValue {
  readonly typename: string;
  readonly record: unknown;
}

interface ResolverContext {
  readonly store: Store;
  readonly variables: Record<string, unknown>;
  readonly fragments: Map<string, FragmentNode>;
  readonly errors: { message: string }[];
}

function valueOf(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name] === undefined ? null : variables[node.name];
    case 'int':
    case 'float':
    case 'string':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((entry) => valueOf(entry, variables));
    case 'object': {
      const record: Record<string, unknown> = {};
      for (const field of node.fields) record[field.name] = valueOf(field.value, variables);
      return record;
    }
  }
}

function argsOf(field: Extract<SelectionNode, { kind: 'field' }>, variables: Record<string, unknown>): Record<string, unknown> {
  const args: Record<string, unknown> = {};
  for (const arg of field.args) args[arg.name] = valueOf(arg.value, variables);
  return args;
}

function isSkipped(directives: DirectiveNode[], variables: Record<string, unknown>): boolean {
  for (const directive of directives) {
    const ifArg = directive.args.find((arg) => arg.name === 'if');
    const value = ifArg === undefined ? false : valueOf(ifArg.value, variables);
    if (directive.name === 'skip' && value === true) return true;
    if (directive.name === 'include' && value !== true) return true;
  }
  return false;
}

function asString(value: unknown): string | null {
  return typeof value === 'string' ? value : null;
}

function asInt(value: unknown): number | null {
  if (typeof value === 'number' && Number.isSafeInteger(value)) return value;
  if (typeof value === 'string' && /^-?\d+$/.test(value)) return Number.parseInt(value, 10);
  return null;
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
}

function issueState(item: Item): string {
  return item.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(item: Item): string {
  if (item.state === 'open') return 'OPEN';
  return item.merged ? 'MERGED' : 'CLOSED';
}

function mergeableState(store: Store, repo: Repo, item: Item): string {
  if (item.state !== 'open') return 'UNKNOWN';
  return store.isMergeable(repo, item) ? 'MERGEABLE' : 'CONFLICTING';
}

function userValue(): TypedValue {
  return { typename: 'User', record: VIEWER };
}

function repoValue(repo: Repo): TypedValue {
  return { typename: 'Repository', record: repo };
}

function itemValue(item: Item): TypedValue {
  return { typename: item.kind === 'pull' ? 'PullRequest' : 'Issue', record: item };
}

function commentValue(comment: Comment): TypedValue {
  return { typename: 'IssueComment', record: comment };
}

/** Order items by number for a connection: CREATED_AT/UPDATED_AT/COMMENTS all reduce to number order. */
function orderedItems(items: Item[], orderBy: unknown): Item[] {
  const record = asRecord(orderBy);
  const direction = record === null ? 'DESC' : String(record.direction ?? 'DESC');
  const sorted = [...items].sort((a, b) => a.number - b.number);
  return direction === 'ASC' ? sorted : sorted.reverse();
}

function statesFilter(states: unknown): Set<string> | null {
  if (!Array.isArray(states)) return null;
  const wanted = new Set<string>();
  for (const entry of states) if (typeof entry === 'string') wanted.add(entry);
  return wanted;
}

/** Cursor pagination over an ordered list: the cursor names the last number of the previous page. */
function paginateConnection<T extends { number: number }>(ordered: T[], first: number | null, after: unknown): { nodes: T[]; hasNextPage: boolean; endCursor: string | null } {
  let start = 0;
  const cursor = asString(after);
  if (cursor !== null && cursor.startsWith('cursor:')) {
    const number = Number.parseInt(cursor.slice('cursor:'.length), 10);
    const index = ordered.findIndex((entry) => entry.number === number);
    if (index !== -1) start = index + 1;
  }
  const limit = first === null || first < 0 ? ordered.length : first;
  const nodes = ordered.slice(start, start + limit);
  const hasNextPage = start + nodes.length < ordered.length;
  const endCursor = nodes.length === 0 ? null : `cursor:${(nodes[nodes.length - 1] as T).number}`;
  return { nodes, hasNextPage, endCursor };
}

interface ConnectionValue {
  readonly typename: string;
  readonly nodes: TypedValue[];
  readonly hasNextPage: boolean;
  readonly endCursor: string | null;
  readonly hasPreviousPage: boolean;
  readonly startCursor: string | null;
}

function connectionValue(typename: string, nodes: TypedValue[], page: { hasNextPage: boolean; endCursor: string | null }, after: unknown): ConnectionValue {
  return {
    typename,
    nodes,
    hasNextPage: page.hasNextPage,
    endCursor: page.endCursor,
    hasPreviousPage: after !== null && after !== undefined,
    startCursor: null,
  };
}

// --------------------------------------------------------------------------- introspection

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'body', 'state', 'author', 'labels', 'comments', 'createdAt', 'updatedAt', 'closedAt', 'repository', 'url'],
  PullRequest: ['id', 'number', 'title', 'body', 'state', 'headRefName', 'baseRefName', 'mergeable', 'merged', 'files', 'author', 'labels', 'comments', 'createdAt', 'updatedAt', 'closedAt', 'mergedAt', 'repository', 'url'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'isPrivate', 'createdAt', 'updatedAt', 'url'],
  User: ['id', 'login', 'name', 'email'],
};

const ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'COMMIT', 'CODE', 'DISCUSSION', 'LABEL', 'MILESTONE', 'PACKAGE', 'WIKI'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  OrderDirection: ['ASC', 'DESC'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
};

// --------------------------------------------------------------------------- field resolution

type FieldResult = JsonValue | TypedValue | ConnectionValue | FieldResult[] | null;

/**
 * Resolve one field on a typed value. Returns the field's value; unknown fields record an error
 * and resolve to a sentinel the caller omits from the response object.
 */
const UNKNOWN = Symbol('unknown-field');

function resolveField(ctx: ResolverContext, parent: TypedValue | ConnectionValue | null, fieldName: string, args: Record<string, unknown>): FieldResult | typeof UNKNOWN {
  const store = ctx.store;

  // Root fields (Query / Mutation) have no parent.
  if (parent === null) {
    if (fieldName === 'viewer') return userValue();
    if (fieldName === '__type') {
      const name = asString(args.name);
      if (name === null) return null;
      if (TYPE_FIELDS[name] === undefined && ENUM_VALUES[name] === undefined) return null;
      return { typename: '__Type', record: { name, fields: TYPE_FIELDS[name] ?? [], enumValues: ENUM_VALUES[name] ?? [] } };
    }
    if (fieldName === 'repository') {
      const owner = asString(args.owner);
      const name = asString(args.name);
      if (owner === null || name === null) return null;
      const repo = store.getRepo(owner, name);
      return repo === undefined ? null : repoValue(repo);
    }
    if (fieldName === 'node') {
      const id = asString(args.id);
      if (id === null) return null;
      const found = store.nodeByGlobalId(id);
      if (found === undefined) return null;
      if (found.kind === 'repo') return repoValue(found.repo);
      if (found.kind === 'item') return itemValue(found.item);
      return commentValue(found.comment);
    }
    if (fieldName === 'createIssue') return createIssuePayload(ctx, args);
    if (fieldName === 'closeIssue') return closeIssuePayload(ctx, args);
    if (fieldName === 'createPullRequest') return createPullRequestPayload(ctx, args);
    if (fieldName === 'mergePullRequest') return mergePullRequestPayload(ctx, args);
    if (fieldName === 'addComment') return addCommentPayload(ctx, args);
    return UNKNOWN;
  }

  if (parent.typename === '__Type') {
    const record = parent.record as { name: string; fields: string[]; enumValues: string[] };
    if (fieldName === 'name') return record.name;
    if (fieldName === 'fields') return record.fields.map((name) => ({ typename: '__Field', record: { name } }));
    if (fieldName === 'enumValues') return record.enumValues.map((name) => ({ typename: '__EnumValue', record: { name } }));
    return UNKNOWN;
  }
  if (parent.typename === '__Field' || parent.typename === '__EnumValue') {
    if (fieldName === 'name') return (parent.record as { name: string }).name;
    return UNKNOWN;
  }

  if (parent.typename === 'User') {
    if (fieldName === 'login') return VIEWER.login;
    if (fieldName === 'id') return `U_${VIEWER.id}`;
    if (fieldName === 'name') return VIEWER.name;
    if (fieldName === 'email') return VIEWER.email;
    return UNKNOWN;
  }

  if (parent.typename === 'Ref') {
    if (fieldName === 'name') return (parent.record as { name: string }).name;
    return UNKNOWN;
  }

  if (parent.typename === 'Repository') {
    const repo = parent.record as Repo;
    if (fieldName === 'id') return globalRepoId(repo);
    if (fieldName === 'databaseId') return repo.id;
    if (fieldName === 'name') return repo.name;
    if (fieldName === 'nameWithOwner') return repo.fullName;
    if (fieldName === 'description') return repo.description;
    if (fieldName === 'isPrivate') return repo.isPrivate;
    if (fieldName === 'createdAt') return repo.createdAt;
    if (fieldName === 'updatedAt') return repo.updatedAt;
    if (fieldName === 'owner') return userValue();
    if (fieldName === 'defaultBranchRef') {
      const tip = store.getRef(repo, `refs/heads/${repo.defaultBranch}`);
      return tip === undefined ? null : { typename: 'Ref', record: { name: repo.defaultBranch } };
    }
    if (fieldName === 'viewerPermission') return 'ADMIN';
    if (fieldName === 'hasIssuesEnabled') return true;
    if (fieldName === 'issue') {
      const number = asInt(args.number);
      if (number === null) return null;
      const item = store.getItem(repo, number);
      return item === undefined || item.kind !== 'issue' ? null : itemValue(item);
    }
    if (fieldName === 'pullRequest') {
      const number = asInt(args.number);
      if (number === null) return null;
      const item = store.getItem(repo, number);
      return item === undefined || item.kind !== 'pull' ? null : itemValue(item);
    }
    if (fieldName === 'issues' || fieldName === 'pullRequests') {
      const kind = fieldName === 'issues' ? 'issue' : 'pull';
      let items = store.listItems(repo).filter((item) => item.kind === kind);
      const wanted = statesFilter(args.states);
      if (wanted !== null) {
        items = items.filter((item) => wanted.has(kind === 'issue' ? issueState(item) : pullState(item)));
      } else {
        items = items.filter((item) => item.state === 'open');
      }
      const ordered = orderedItems(items, args.orderBy);
      const page = paginateConnection(ordered, asInt(args.first), args.after);
      const nodes = page.nodes.map(itemValue);
      return connectionValue(fieldName === 'issues' ? 'IssueConnection' : 'PullRequestConnection', nodes, page, args.after);
    }
    return UNKNOWN;
  }

  if (parent.typename === 'Issue' || parent.typename === 'PullRequest') {
    const item = parent.record as Item;
    const repo = ctx.store.repoOfItem(item);
    if (fieldName === 'id') return globalItemId(item);
    if (fieldName === 'number') return item.number;
    if (fieldName === 'title') return item.title;
    if (fieldName === 'body') return item.body;
    if (fieldName === 'state') return parent.typename === 'Issue' ? issueState(item) : pullState(item);
    if (fieldName === 'createdAt') return item.createdAt;
    if (fieldName === 'updatedAt') return item.updatedAt;
    if (fieldName === 'closedAt') return item.closedAt;
    if (fieldName === 'author') return userValue();
    if (fieldName === 'repository') return repo === undefined ? null : repoValue(repo);
    if (parent.typename === 'PullRequest') {
      if (fieldName === 'headRefName') return item.headRef ?? '';
      if (fieldName === 'baseRefName') return item.baseRef ?? '';
      if (fieldName === 'merged') return item.merged;
      if (fieldName === 'mergedAt') return item.mergedAt;
      if (fieldName === 'mergeable') return repo === undefined ? 'UNKNOWN' : mergeableState(store, repo, item);
      if (fieldName === 'files') {
        if (repo === undefined) return connectionValue('PullRequestChangedFileConnection', [], { hasNextPage: false, endCursor: null }, null);
        const baseTip = item.baseRef === null ? undefined : store.resolveRef(repo, item.baseRef);
        const headTip = item.headRef === null ? undefined : store.resolveRef(repo, item.headRef);
        if (baseTip === undefined || headTip === undefined) {
          return connectionValue('PullRequestChangedFileConnection', [], { hasNextPage: false, endCursor: null }, null);
        }
        const base = store.mergeBase(repo, baseTip, headTip);
        const files = store.diffFiles(repo, base, headTip);
        const first = asInt(args.first);
        const sliced = first === null ? files : files.slice(0, Math.max(0, first));
        const nodes = sliced.map((file) => ({ typename: 'PullRequestChangedFile', record: { path: file.filename } }));
        return connectionValue('PullRequestChangedFileConnection', nodes, { hasNextPage: false, endCursor: null }, null);
      }
    }
    return UNKNOWN;
  }

  if (parent.typename === 'PullRequestChangedFile') {
    if (fieldName === 'path') return (parent.record as { path: string }).path;
    return UNKNOWN;
  }

  if (parent.typename === 'IssueComment') {
    const comment = parent.record as Comment;
    if (fieldName === 'id') return globalCommentId(comment);
    if (fieldName === 'body') return comment.body;
    if (fieldName === 'createdAt') return comment.createdAt;
    if (fieldName === 'updatedAt') return comment.updatedAt;
    if (fieldName === 'author') return userValue();
    return UNKNOWN;
  }

  if (parent.typename === 'IssueConnection' || parent.typename === 'PullRequestConnection' || parent.typename === 'PullRequestChangedFileConnection') {
    const connection = parent as ConnectionValue;
    if (fieldName === 'nodes') return connection.nodes;
    if (fieldName === 'pageInfo') return { typename: 'PageInfo', record: connection };
    if (fieldName === 'totalCount') return connection.nodes.length;
    return UNKNOWN;
  }

  if (parent.typename === 'PageInfo') {
    const connection = parent.record as ConnectionValue;
    if (fieldName === 'hasNextPage') return connection.hasNextPage;
    if (fieldName === 'hasPreviousPage') return connection.hasPreviousPage;
    if (fieldName === 'endCursor') return connection.endCursor;
    if (fieldName === 'startCursor') return connection.startCursor;
    return UNKNOWN;
  }

  if (parent.typename === 'CreateIssuePayload' || parent.typename === 'CloseIssuePayload') {
    if (fieldName === 'issue') {
      const item = (parent.record as { item: Item }).item;
      return itemValue(item);
    }
    return UNKNOWN;
  }
  if (parent.typename === 'CreatePullRequestPayload' || parent.typename === 'MergePullRequestPayload') {
    if (fieldName === 'pullRequest') {
      const item = (parent.record as { item: Item }).item;
      return itemValue(item);
    }
    return UNKNOWN;
  }
  if (parent.typename === 'AddCommentPayload') {
    if (fieldName === 'commentEdge') return { typename: 'IssueCommentEdge', record: parent.record };
    return UNKNOWN;
  }
  if (parent.typename === 'IssueCommentEdge') {
    if (fieldName === 'node') {
      const comment = (parent.record as { comment: Comment }).comment;
      return commentValue(comment);
    }
    return UNKNOWN;
  }

  return UNKNOWN;
}

// --------------------------------------------------------------------------- mutations

function mutationError(ctx: ResolverContext, message: string): null {
  ctx.errors.push({ message });
  return null;
}

function createIssuePayload(ctx: ResolverContext, args: Record<string, unknown>): TypedValue | null {
  const input = asRecord(args.input);
  if (input === null) return mutationError(ctx, 'createIssue requires an input object');
  const repo = input.repositoryId === undefined ? undefined : ctx.store.repoByGlobalId(String(input.repositoryId));
  if (repo === undefined) return mutationError(ctx, 'createIssue: repository not found');
  const title = asString(input.title);
  if (title === null || title === '') return mutationError(ctx, 'createIssue: title is required');
  try {
    const item = ctx.store.createItem(repo, 'issue', { title, body: asString(input.body) ?? '' });
    return { typename: 'CreateIssuePayload', record: { item } };
  } catch (err) {
    return mutationError(ctx, err instanceof StoreError ? err.message : 'createIssue failed');
  }
}

function closeIssuePayload(ctx: ResolverContext, args: Record<string, unknown>): TypedValue | null {
  const input = asRecord(args.input);
  if (input === null) return mutationError(ctx, 'closeIssue requires an input object');
  const found = input.issueId === undefined ? undefined : ctx.store.itemByGlobalId(String(input.issueId));
  if (found === undefined) return mutationError(ctx, 'closeIssue: issue not found');
  found.item.state = 'closed';
  found.item.closedAt = trialNowIso();
  found.item.updatedAt = trialNowIso();
  ctx.store.touch(found.repo);
  return { typename: 'CloseIssuePayload', record: { item: found.item } };
}

function createPullRequestPayload(ctx: ResolverContext, args: Record<string, unknown>): TypedValue | null {
  const input = asRecord(args.input);
  if (input === null) return mutationError(ctx, 'createPullRequest requires an input object');
  const repo = input.repositoryId === undefined ? undefined : ctx.store.repoByGlobalId(String(input.repositoryId));
  if (repo === undefined) return mutationError(ctx, 'createPullRequest: repository not found');
  const title = asString(input.title);
  const head = asString(input.headRefName);
  const base = asString(input.baseRefName);
  if (title === null || title === '') return mutationError(ctx, 'createPullRequest: title is required');
  if (head === null || ctx.store.resolveRef(repo, head) === undefined) return mutationError(ctx, 'createPullRequest: head branch not found');
  if (base === null || ctx.store.resolveRef(repo, base) === undefined) return mutationError(ctx, 'createPullRequest: base branch not found');
  try {
    const item = ctx.store.createItem(repo, 'pull', { title, body: asString(input.body) ?? '', headRef: head, baseRef: base });
    return { typename: 'CreatePullRequestPayload', record: { item } };
  } catch (err) {
    return mutationError(ctx, err instanceof StoreError ? err.message : 'createPullRequest failed');
  }
}

function mergePullRequestPayload(ctx: ResolverContext, args: Record<string, unknown>): TypedValue | null {
  const input = asRecord(args.input);
  if (input === null) return mutationError(ctx, 'mergePullRequest requires an input object');
  const found = input.pullRequestId === undefined ? undefined : ctx.store.itemByGlobalId(String(input.pullRequestId));
  if (found === undefined || found.item.kind !== 'pull') return mutationError(ctx, 'mergePullRequest: pull request not found');
  if (found.item.state !== 'open') return mutationError(ctx, 'mergePullRequest: pull request is already closed');
  const sha = ctx.store.mergePullRequest(found.repo, found.item, `${found.item.title} (#${found.item.number})`);
  if (sha === null) return mutationError(ctx, 'mergePullRequest: pull request cannot be merged due to conflicts');
  return { typename: 'MergePullRequestPayload', record: { item: found.item } };
}

function addCommentPayload(ctx: ResolverContext, args: Record<string, unknown>): TypedValue | null {
  const input = asRecord(args.input);
  if (input === null) return mutationError(ctx, 'addComment requires an input object');
  const found = input.subjectId === undefined ? undefined : ctx.store.itemByGlobalId(String(input.subjectId));
  if (found === undefined) return mutationError(ctx, 'addComment: subject not found');
  const body = asString(input.body);
  if (body === null || body === '') return mutationError(ctx, 'addComment: body is required');
  const comment = ctx.store.addComment(found.repo, found.item, body);
  return { typename: 'AddCommentPayload', record: { comment } };
}

// --------------------------------------------------------------------------- selection execution

function executeSelectionSet(
  ctx: ResolverContext,
  parent: TypedValue | ConnectionValue | null,
  parentTypename: string,
  selections: SelectionNode[],
): Record<string, JsonValue> {
  const result: Record<string, JsonValue> = {};
  for (const selection of selections) {
    if (selection.kind === 'fragmentSpread') {
      if (isSkipped(selection.directives, ctx.variables)) continue;
      const fragment = ctx.fragments.get(selection.name);
      if (fragment === undefined || (fragment.typeCondition !== parentTypename && fragment.typeCondition !== 'Node')) continue;
      Object.assign(result, executeSelectionSet(ctx, parent, parentTypename, fragment.selectionSet));
      continue;
    }
    if (selection.kind === 'inlineFragment') {
      if (isSkipped(selection.directives, ctx.variables)) continue;
      if (selection.typeCondition !== null && selection.typeCondition !== parentTypename && selection.typeCondition !== 'Node') continue;
      Object.assign(result, executeSelectionSet(ctx, parent, parentTypename, selection.selectionSet));
      continue;
    }
    if (isSkipped(selection.directives, ctx.variables)) continue;
    const key = selection.alias ?? selection.name;
    const args = argsOf(selection, ctx.variables);
    const value = resolveField(ctx, parent, selection.name, args);
    if (value === UNKNOWN) {
      ctx.errors.push({ message: `Cannot query field "${selection.name}" on type "${parentTypename}".` });
      continue;
    }
    result[key] = completeValue(ctx, value, selection.selectionSet);
  }
  return result;
}

function completeValue(ctx: ResolverContext, value: FieldResult, selectionSet: SelectionNode[] | null): JsonValue {
  if (value === null || typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') return value;
  if (Array.isArray(value)) {
    return value.map((entry) => completeValue(ctx, entry as FieldResult, selectionSet));
  }
  const typed = value as TypedValue | ConnectionValue;
  if (selectionSet === null) return null;
  if ('nodes' in typed) {
    return executeSelectionSet(ctx, typed, typed.typename, selectionSet) as unknown as JsonValue;
  }
  return executeSelectionSet(ctx, typed, typed.typename, selectionSet) as unknown as JsonValue;
}

function executeOperation(store: Store, document: DocumentNode, operationName: string | undefined, variables: Record<string, unknown>): { data: Record<string, JsonValue> | null; errors: { message: string }[] } {
  const errors: { message: string }[] = [];
  let operation: OperationNode | undefined;
  if (operationName !== undefined) {
    operation = document.operations.find((entry) => entry.name === operationName);
    if (operation === undefined) return { data: null, errors: [{ message: `Unknown operation named "${operationName}".` }] };
  } else if (document.operations.length === 1) {
    operation = document.operations[0] as OperationNode;
  } else {
    return { data: null, errors: [{ message: 'Must provide an operation name when the document holds more than one operation.' }] };
  }
  const resolvedVariables: Record<string, unknown> = { ...variables };
  for (const variable of operation.variables) {
    if (resolvedVariables[variable.name] === undefined && variable.defaultValue !== null) {
      resolvedVariables[variable.name] = valueOf(variable.defaultValue, resolvedVariables);
    }
  }
  const ctx: ResolverContext = { store, variables: resolvedVariables, fragments: document.fragments, errors };
  const rootTypename = operation.operation === 'mutation' ? 'Mutation' : 'Query';
  const data = executeSelectionSet(ctx, null, rootTypename, operation.selectionSet);
  return { data, errors };
}

// --------------------------------------------------------------------------- entry point

export interface GraphQLReply {
  readonly status: number;
  readonly headers: Record<string, string>;
  readonly body: Buffer;
}

function executeOne(store: Store, entry: unknown): Record<string, unknown> {
  const record = asRecord(entry);
  if (record === null) return { data: null, errors: [{ message: 'Must provide a query object.' }] };
  const query = record.query;
  if (typeof query !== 'string') return { data: null, errors: [{ message: 'Must provide query string.' }] };
  const variables = asRecord(record.variables) ?? {};
  const operationName = typeof record.operationName === 'string' ? record.operationName : undefined;
  let document: DocumentNode;
  try {
    document = new Parser(lex(query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'Syntax error.' }] };
  }
  const { data, errors } = executeOperation(store, document, operationName, variables);
  return errors.length === 0 ? { data } : { data, errors };
}

/**
 * Handle one POST /api/graphql body (a single operation object, or a JSON array of them for
 * batching). Always answers HTTP 200 with GraphQL envelopes; transport-level problems (auth,
 * malformed JSON) are the app layer's 401/400.
 */
export function handleGraphQL(store: Store, body: unknown): GraphQLReply {
  const headers = { 'content-type': 'application/json; charset=utf-8', 'x-github-request-id': store.nextRequestId() };
  if (Array.isArray(body)) {
    const results = body.map((entry) => executeOne(store, entry));
    return { status: 200, headers, body: Buffer.from(`${JSON.stringify(results)}\n`, 'utf8') };
  }
  if (asRecord(body) === null) {
    return { status: 400, headers, body: Buffer.from(`${JSON.stringify({ message: 'Problems parsing JSON' })}\n`, 'utf8') };
  }
  const result = executeOne(store, body);
  return { status: 200, headers, body: Buffer.from(`${JSON.stringify(result)}\n`, 'utf8') };
}
