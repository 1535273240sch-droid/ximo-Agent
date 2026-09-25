import { Children, isValidElement, useEffect, useRef, useState } from 'react'
import type { ComponentProps, CSSProperties, ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter'
import { Check, Copy } from 'lucide-react'

const themeAgnosticPrismStyle: Record<string, CSSProperties> = {
  'code[class*="language-"]': {
    color: 'rgb(var(--c-ink))',
    fontFamily: 'var(--font-mono)',
    fontSize: '12.5px',
    lineHeight: '1.6',
    direction: 'ltr',
    textAlign: 'left',
    whiteSpace: 'pre',
    wordSpacing: 'normal',
    wordBreak: 'normal',
    tabSize: 2,
    hyphens: 'none',
    background: 'transparent'
  },
  'pre[class*="language-"]': {
    color: 'rgb(var(--c-ink))',
    fontFamily: 'var(--font-mono)',
    fontSize: '12.5px',
    lineHeight: '1.6',
    direction: 'ltr',
    textAlign: 'left',
    whiteSpace: 'pre',
    wordSpacing: 'normal',
    wordBreak: 'normal',
    tabSize: 2,
    hyphens: 'none',
    padding: '0.85rem 1rem',
    margin: 0,
    overflow: 'auto',
    background: 'transparent'
  },
  comment: { color: 'rgb(var(--c-ink-faint))', fontStyle: 'italic' },
  prolog: { color: 'rgb(var(--c-ink-faint))' },
  doctype: { color: 'rgb(var(--c-ink-faint))' },
  cdata: { color: 'rgb(var(--c-ink-faint))' },
  punctuation: { color: 'rgb(var(--c-ink-muted))' },
  property: { color: 'rgb(var(--c-accent))' },
  tag: { color: 'rgb(var(--c-accent))' },
  boolean: { color: 'rgb(var(--c-accent))', fontWeight: 600 },
  number: { color: 'rgb(var(--c-accent))' },
  constant: { color: 'rgb(var(--c-accent))' },
  symbol: { color: 'rgb(var(--c-accent))' },
  deleted: { color: 'rgb(var(--c-danger))' },
  selector: { color: 'rgb(var(--c-success))' },
  'attr-name': { color: 'rgb(var(--c-ink))' },
  string: { color: 'rgb(var(--c-success))' },
  char: { color: 'rgb(var(--c-success))' },
  builtin: { color: 'rgb(var(--c-accent))' },
  inserted: { color: 'rgb(var(--c-success))' },
  operator: { color: 'rgb(var(--c-ink-muted))' },
  entity: { color: 'rgb(var(--c-ink))' },
  url: { color: 'rgb(var(--c-accent))' },
  variable: { color: 'rgb(var(--c-ink))' },
  atrule: { color: 'rgb(var(--c-accent))', fontWeight: 600 },
  'attr-value': { color: 'rgb(var(--c-success))' },
  keyword: { color: 'rgb(var(--c-accent))', fontWeight: 600 },
  function: { color: 'rgb(var(--c-ink))', fontWeight: 600 },
  'class-name': { color: 'rgb(var(--c-ink))', fontWeight: 600 },
  regex: { color: 'rgb(var(--c-warning))' },
  important: { color: 'rgb(var(--c-warning))', fontWeight: 600 }
}

interface MarkdownProps {
  content: string
}

export function Markdown({ content }: MarkdownProps): React.JSX.Element {
  const components: Components = {
    a({ href, children, ...rest }) {
      return (
        <a href={href} target="_blank" rel="noreferrer" {...rest}>
          {children}
        </a>
      )
    },
    pre({ children }) {
      return <CodeBlockContainer>{children}</CodeBlockContainer>
    },
    code(props) {
      const { children, className } = props
      const isBlock = Boolean(className && className.includes('language-'))
      if (isBlock) {
        return <code className={className}>{children}</code>
      }
      return (
        <code className="rounded border border-border/[0.12] bg-surface px-1.5 py-0.5 font-mono text-[12px] text-ink">
          {children}
        </code>
      )
    }
  }

  return (
    <div className="prose-glass max-w-none">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={components}>
        {content}
      </ReactMarkdown>
    </div>
  )
}

function CodeBlockContainer({ children }: { children: ReactNode }): React.JSX.Element {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<NodeJS.Timeout | null>(null)

  useEffect(() => {
    return () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    }
  }, [])

  const extractCode = (node: ReactNode): { codeText: string; language: string } => {
    let codeText = ''
    let language = 'text'

    Children.forEach(node, (child) => {
      if (isValidElement<ComponentProps<'code'>>(child)) {
        const className = child.props.className ?? ''
        const match = /language-(\w+)/.exec(className)
        if (match) language = match[1]

        const raw = child.props.children
        if (typeof raw === 'string') {
          codeText = raw
        } else if (Array.isArray(raw)) {
          codeText = raw.join('')
        }
      }
    })

    return { codeText: codeText.replace(/\n$/, ''), language }
  }

  const { codeText, language } = extractCode(children)

  const handleCopy = async (): Promise<void> => {
    if (!codeText) return
    try {
      await navigator.clipboard.writeText(codeText)
      setCopied(true)
      if (timerRef.current) clearTimeout(timerRef.current)
      timerRef.current = setTimeout(() => setCopied(false), 2000)
    } catch {
      // 忽略
    }
  }

  return (
    <div className="glass-inset my-3 overflow-hidden rounded-[8px]">
      <div className="flex h-8 items-center justify-between border-b border-border/[0.08] px-3 bg-surface">
        <span className="font-mono text-[12px] font-semibold text-ink-muted uppercase">
          {language}
        </span>
        <button
          type="button"
          onClick={handleCopy}
          className="flex items-center gap-1 rounded px-2 py-0.5 text-[12px] text-ink-muted transition-colors hover:text-ink"
          title="复制全部代码"
        >
          {copied ? <Check size={12} className="text-success" /> : <Copy size={12} />}
          <span>{copied ? '已复制' : '复制代码'}</span>
        </button>
      </div>

      <div className="overflow-x-auto text-[12.5px]">
        <SyntaxHighlighter
          language={language}
          style={themeAgnosticPrismStyle}
          customStyle={{
            margin: 0,
            padding: '0.85rem 1rem',
            background: 'transparent',
            fontSize: '12.5px',
            lineHeight: '1.6'
          }}
          codeTagProps={{
            style: {
              fontFamily: 'var(--font-mono)'
            }
          }}
        >
          {codeText}
        </SyntaxHighlighter>
      </div>
    </div>
  )
}
