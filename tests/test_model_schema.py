import unittest

from model_schema import model_data, runtime_context, scalar_field


class ModelSchemaTests(unittest.TestCase):
    def test_tagged_data_requires_exact_marker(self):
        self.assertEqual(model_data('<runtime_notification>\ntype: job_exit\n</runtime_notification>',
                                    'runtime_notification'), 'type: job_exit')
        for text in ('{"type":"job_exit"}', '<runtime_notification>\n---\n</other>',
                     'prefix\n<runtime_notification>\ntype: job_exit\n</runtime_notification>'):
            with self.assertRaises(AssertionError):
                model_data(text, 'runtime_notification')

    def test_fixture_scalar_types_and_literal_answer(self):
        data = ('type: child_turn_finished\npersistent: false\nresult_entry_id: 9007199254740993\n'
                'answer: |-\n  ## Audit complete\n\n  The evidence reports **42 µm**. Unicode: α → β.\n'
                'child_state: closed\nquoted: "with\\nnewline"\n')
        self.assertEqual(scalar_field(data, 'type'), 'child_turn_finished')
        self.assertIs(scalar_field(data, 'persistent'), False)
        self.assertEqual(scalar_field(data, 'result_entry_id'), 9007199254740993)
        self.assertEqual(scalar_field(data, 'answer'),
                         '## Audit complete\n\nThe evidence reports **42 µm**. Unicode: α → β.')
        self.assertEqual(scalar_field(data, 'quoted'), 'with\nnewline')
        for name in ('missing',):
            with self.assertRaises(AssertionError):
                scalar_field(data, name)
        with self.assertRaises(AssertionError):
            scalar_field('type: a\ntype: b', 'type')

    def test_runtime_context_baselines_and_flat_updates(self):
        def item(text):
            return {'role': 'developer', 'content': [{'type': 'input_text', 'text': text}]}
        body = {'input': [item('<runtime_context>\nactor: main\nimage_click: true\n</runtime_context>'),
                          item('<runtime_context_update>\nlive_jobs: []\n</runtime_context_update>'),
                          item('<runtime_instruction>\nContinue.\n</runtime_instruction>')]}
        self.assertEqual(runtime_context(body), {'actor': 'main', 'image_click': True})
        body['input'].append(item('<runtime_context>\nactor: main/child_test\nimage_click: false\n</runtime_context>'))
        self.assertEqual(runtime_context(body), {'actor': 'main/child_test', 'image_click': False})
        body['input'].append(item('<runtime_context_update>\nset:\n  live_jobs: []\n</runtime_context_update>'))
        with self.assertRaises(AssertionError):
            runtime_context(body)


if __name__ == '__main__':
    unittest.main()
