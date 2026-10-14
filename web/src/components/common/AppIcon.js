import { defineComponent, h } from 'vue'
import { resolveAppIcon } from './appIcons'

/** Existing <t-icon name="…"> calls now render local SVG paths, even without network access. */
export default defineComponent({
  name: 'AppIcon',
  inheritAttrs: false,
  props: {
    name: { type: String, default: '' },
    size: { type: [String, Number], default: undefined },
  },
  emits: ['click'],
  setup(props, { attrs, emit }) {
    return () => h(resolveAppIcon(props.name), {
      ...attrs,
      size: typeof props.size === 'number' ? `${props.size}px` : props.size,
      'data-icon-name': props.name,
      'aria-hidden': attrs['aria-label'] || attrs.role === 'img' ? undefined : 'true',
      focusable: 'false',
      // Concrete TDesign icons wrap native events in { e }; keep DOM event modifiers working.
      onClick: event => emit('click', event?.e || event),
    })
  },
})
