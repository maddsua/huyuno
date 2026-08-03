<script setup lang="ts">
const emit = defineEmits<{
	(e: 'exit'): void;
}>();
</script>

<template>
	<div class="editor-header">

		<button class="home" type="button" title="Go home" @click="emit('exit')"></button>

		<div class="primary-group">

			<div class="summary-group">

				<div class="slot summary">

					<slot name="meta">
						<span class="placeholder">
							[Meta]
						</span>
					</slot>

				</div>

				<div class="slot menu">

					<slot name="ribbon">
						<span class="placeholder">
							[Ribbon]
						</span>
					</slot>
				</div>
			</div>

			<div class="edit-group">

				<div class="slot color">
					<slot name="colors">
						<span class="placeholder">
							[Colors]
						</span>
					</slot>
				</div>

				<div class="slot insert">
					<slot name="insert">
						<span class="placeholder">
							[Insert]
						</span>
					</slot>
				</div>

			</div>

			<div class="slot publishing">
				<slot name="autosave">
					<span class="placeholder">
						[Autosave]
					</span>
				</slot>
				<slot name="publish">
					<span class="placeholder">
						[Publish]
					</span>
				</slot>
			</div>
		</div>

	</div>
</template>

<style lang="scss" scoped>

	.editor-header {
		position: relative;
		z-index: 100;
		display: flex;
		flex-flow: row nowrap;
		gap: 0.5rem;
		padding: 0.5rem 1rem;
		background-color: var(--app-theme-ghostly-glow);
	}

	.primary-group {
		display: flex;
		flex-flow: row nowrap;
		gap: 0.5rem;
		flex-grow: 1;
		align-items: start;
		justify-content: space-between;
	}

	.summary-group {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
	}

	.edit-group {
		display: flex;
		flex-flow: row nowrap;
		gap: 2rem;
		align-items: end;
		height: 100%;
		padding: 0.25rem;
	}

	.slot {
		display: flex;

		&.summary {
			flex-direction: column;
			gap: 0.25rem;
		}

		&.menu {
			flex-flow: row nowrap;
			align-items: center;
			gap: 0.25rem;
		}

		&.insert, &.color {
			flex-flow: row wrap;
			gap: 0.25rem;
		}

		&.publishing {
			display: flex;
			flex-flow: row nowrap;
			gap: 2rem;
			height: fit-content;
			padding: 0.25rem;
		}
	}

	span.placeholder {
		color: var(--app-theme-mysterious-white);
		font-size: 0.75rem;
	}

	button.home {
		display: block;
		width: 3rem;
		height: 3rem;
		outline: none;
		border: none;
		background: unset;
		background-image: url(/public/static/icons/app-icon.svg);
		background-size: contain;
		background-position: center;
		background-repeat: no-repeat;
		opacity: 0.8;
		transition: all 150ms ease;

		&:hover {
			cursor: pointer;
			opacity: 1;
		}
	}

</style>
