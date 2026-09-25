import { useState } from 'react'
import { Save, Tag, Trash2, X } from 'lucide-react'
import type { KnowledgeEntry, KnowledgeMode } from './KnowledgeView'

interface EntryEditorProps {
  entry?: KnowledgeEntry
  mode: KnowledgeMode
  onSave: (entry: Omit<KnowledgeEntry, 'id' | 'updatedAt'> & { id?: string }) => void
  onCancel?: () => void
  onDelete?: () => void
}

export function EntryEditor({
  entry,
  mode,
  onSave,
  onCancel,
  onDelete
}: EntryEditorProps): React.JSX.Element {
  const [title, setTitle] = useState(entry?.title ?? '')
  const [content, setContent] = useState(entry?.content ?? '')
  const [tags, setTags] = useState<string[]>(entry?.tags ?? [])
  const [tagInput, setTagInput] = useState('')
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false)

  const isEditing = Boolean(entry)
  const isDirty =
    title !== (entry?.title ?? '') ||
    content !== (entry?.content ?? '') ||
    tags.join(',') !== (entry?.tags?.join(',') ?? '')

  const handleAddTag = (): void => {
    const trimmed = tagInput.trim().replace(/^#/, '')
    if (trimmed && !tags.includes(trimmed)) {
      setTags([...tags, trimmed])
      setTagInput('')
    }
  }

  const handleRemoveTag = (tagToRemove: string): void => {
    setTags(tags.filter((t) => t !== tagToRemove))
  }

  const handleSubmit = (e: React.FormEvent): void => {
    e.preventDefault()
    if (!title.trim()) return
    onSave({
      id: entry?.id,
      title: title.trim(),
      content: content.trim(),
      tags,
      mode,
      source: entry?.source ?? 'user'
    })
  }

  return (
    <form onSubmit={handleSubmit} className="flex h-full flex-col gap-4">
      {/* 头部标题与控制按钮 */}
      <div className="flex items-center justify-between border-b border-border/[0.08] pb-3.5">
        <span className="font-display text-[15px] font-semibold text-ink">
          {isEditing ? '编辑语料条目' : '新建知识规范'}
        </span>

        <div className="flex items-center gap-2">
          {isEditing && onDelete && (
            <>
              {showDeleteConfirm ? (
                <div className="flex items-center gap-1.5 rounded-[6px] border border-danger/30 bg-danger/10 px-2 py-1">
                  <span className="text-[12px] font-medium text-danger">确定删除？</span>
                  <button
                    type="button"
                    onClick={onDelete}
                    className="rounded bg-danger px-2 py-0.5 text-[12px] font-medium text-white"
                  >
                    删除
                  </button>
                  <button
                    type="button"
                    onClick={() => setShowDeleteConfirm(false)}
                    className="text-[12px] text-ink-muted hover:text-ink px-1"
                  >
                    取消
                  </button>
                </div>
              ) : (
                <button
                  type="button"
                  onClick={() => setShowDeleteConfirm(true)}
                  className="flex items-center gap-1 rounded-[6px] border border-border/[0.1] px-2.5 py-1.5 text-[12px] text-danger transition-colors hover:bg-danger/10"
                >
                  <Trash2 size={13} />
                  <span>删除</span>
                </button>
              )}
            </>
          )}

          {onCancel && (
            <button
              type="button"
              onClick={onCancel}
              className="flex items-center gap-1 rounded-[6px] border border-border/[0.1] px-3 py-1.5 text-[12.5px] font-medium text-ink-muted hover:bg-surface-raised hover:text-ink"
            >
              <X size={14} />
              <span>取消</span>
            </button>
          )}

          <button
            type="submit"
            disabled={!title.trim() || (!isDirty && isEditing)}
            className="glass-accent-solid flex items-center gap-1.5 px-3.5 py-1.5 text-[12.5px] font-medium disabled:opacity-40"
          >
            <Save size={13} />
            <span>保存条目</span>
          </button>
        </div>
      </div>

      {/* 标题输入 */}
      <div className="flex flex-col gap-1.5">
        <label className="text-[12px] font-semibold text-ink-muted">条目标题</label>
        <div className="glass-inset px-3 py-2">
          <input
            type="text"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            placeholder="简明描述该规则或知识点…"
            className="w-full bg-transparent font-medium text-[13.5px] text-ink outline-none"
          />
        </div>
      </div>

      {/* 标签管理 */}
      <div className="flex flex-col gap-1.5">
        <label className="text-[12px] font-semibold text-ink-muted">分类标签 (Tags)</label>
        <div className="flex flex-wrap items-center gap-1.5">
          {tags.map((t) => (
            <span
              key={t}
              className="flex items-center gap-1 rounded border border-border/[0.12] bg-canvas px-2 py-0.5 font-mono text-[12px] text-ink"
            >
              <span>#{t}</span>
              <button
                type="button"
                onClick={() => handleRemoveTag(t)}
                className="text-ink-faint hover:text-danger"
              >
                <X size={11} />
              </button>
            </span>
          ))}

          <div className="glass-inset flex items-center gap-1 px-2.5 py-1">
            <Tag size={12} className="text-ink-faint" />
            <input
              type="text"
              value={tagInput}
              onChange={(e) => setTagInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  handleAddTag()
                }
              }}
              placeholder="回车添加标签"
              className="w-[120px] bg-transparent text-[12px] text-ink outline-none"
            />
          </div>
        </div>
      </div>

      {/* 内容大文本 */}
      <div className="flex min-h-0 flex-1 flex-col gap-1.5">
        <label className="text-[12px] font-semibold text-ink-muted">核心正文规范 (Markdown 支持)</label>
        <div className="glass-inset flex min-h-0 flex-1 p-3">
          <textarea
            value={content}
            onChange={(e) => setContent(e.target.value)}
            placeholder="详细陈述知识内容、参考流程或约束细节…"
            className="h-full w-full resize-none bg-transparent font-mono text-[13px] leading-relaxed text-ink outline-none"
          />
        </div>
      </div>
    </form>
  )
}
